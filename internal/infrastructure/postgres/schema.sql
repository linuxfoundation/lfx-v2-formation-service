-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT
--
-- Formation service schema. Applied at startup under a Postgres advisory
-- lock, so it must stay re-runnable: every statement is CREATE ... IF NOT
-- EXISTS and the file is applied twice by a test.
--
-- Four tables. There is deliberately no foreign key to any project table:
-- projects are owned by another service, so project_uid is an opaque
-- reference here.

-- Formation checklist templates. Selection is by priority (lower wins) among
-- published rows, with the first matching rule taking the checklist. Archived
-- rows leave selection but stay readable forever, because formations pin a
-- specific version.
CREATE TABLE IF NOT EXISTS formation_templates (
    uid           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT        NOT NULL,
    version       INT         NOT NULL,
    state         TEXT        NOT NULL DEFAULT 'draft',      -- draft | published | archived
    priority      INT         NOT NULL,                      -- lower wins
    match         TEXT        NOT NULL,                      -- enum-valued rule; 'always' is the fallback
    sections      JSONB       NOT NULL,                      -- [{key, title, items:[...]}]
    author        TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at  TIMESTAMPTZ,

    UNIQUE (name, version)
);

CREATE INDEX IF NOT EXISTS formation_templates_selection_idx
    ON formation_templates (priority) WHERE state = 'published';

-- One formation per project. UNIQUE (project_uid) is the whole 1:1 rule: a
-- duplicate create fails in the database, which is what makes the reconcile
-- loop safe to run on every replica with no reservation key.
--
-- lifecycle carries only what this checklist owns. The project's sub-stage is
-- read from the owning service and never stored, so there is no second source
-- of truth for it.
CREATE TABLE IF NOT EXISTS formations (
    uid              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_uid      TEXT        NOT NULL,
    template_uid     UUID        NOT NULL REFERENCES formation_templates(uid),
    template_version INT         NOT NULL,                   -- pinned; never follows latest
    lifecycle        TEXT        NOT NULL DEFAULT 'live',     -- live | completed | frozen
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ,
    sections         JSONB       NOT NULL DEFAULT '[]',       -- [{key,title}] snapshot; only grows
    revision         BIGINT      NOT NULL DEFAULT 1,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (project_uid)
);

-- Checklist items, one row each. revision is per row rather than per
-- formation so concurrent owners editing different items never contend.
CREATE TABLE IF NOT EXISTS formation_items (
    uid             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    formation_uid   UUID        NOT NULL REFERENCES formations(uid) ON DELETE CASCADE,

    -- identity and placement
    item_key        TEXT        NOT NULL,                    -- snake_case, stable across versions
    section_key     TEXT        NOT NULL,                    -- legal_and_entity | community_and_launch
    position        INT         NOT NULL,

    -- copied from the template at expansion; immutable
    title           TEXT        NOT NULL,
    owner_team      TEXT,                                    -- formation | brand_counsel | it | ...
    gate            BOOLEAN     NOT NULL DEFAULT false,      -- "required for Active"
    -- Defaults true, so expansion must always send an explicit value: Bun
    -- sends a zero-valued notnull column rather than omitting it, which would
    -- silently persist false against a true default. The template side models
    -- it as *bool for that reason -- see TemplateItem.RequiresWriterOrDefault.
    requires_writer BOOLEAN     NOT NULL DEFAULT true,       -- acting on it needs Manage; drives the elevation prompt
    status_source   TEXT        NOT NULL DEFAULT 'manual',   -- manual | platform
    -- Defaults false, unlike requires_writer: a template marks the rows it
    -- genuinely requires rather than excusing the rest. Agreeing with Go's
    -- zero value also keeps an explicit false representable without a pointer.
    is_required     BOOLEAN     NOT NULL DEFAULT false,      -- must be filled in; display metadata, not a gate
    checklist_type  TEXT        NOT NULL DEFAULT 'both',     -- internal | external | both; display metadata only
    platform_check  JSONB,                                   -- {resource_type, min_count}
    action_link     TEXT,                                    -- {{project.uid}} substituted once
    due_date        DATE,                                    -- computed from the template's due_rule

    -- mutable
    status          TEXT        NOT NULL DEFAULT 'not_started',
    assignee        TEXT,                                    -- username only; no email
    note            TEXT,
    skip_reason     TEXT,
    evidence_link   TEXT,                                    -- feeds Quick Links

    -- set by the service
    resolved_ref    JSONB,                                   -- {type, uid} found by a platform check
    sub_items       JSONB       NOT NULL DEFAULT '[]',       -- [{key, title, status}] informational

    revision        BIGINT      NOT NULL DEFAULT 1,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (formation_uid, item_key),                        -- makes the upgrade job idempotent

    CONSTRAINT skip_needs_reason
        CHECK (status <> 'skipped' OR (skip_reason IS NOT NULL AND btrim(skip_reason) <> ''))
);

CREATE INDEX IF NOT EXISTS formation_items_formation_idx ON formation_items (formation_uid, section_key, position);
CREATE INDEX IF NOT EXISTS formation_items_assignee_idx  ON formation_items (assignee) WHERE assignee IS NOT NULL;

-- Append-only activity feed. ULID primary keys give a time-ordered feed and
-- cursor paging from a plain index read. Rows are never updated, so there is
-- no revision column. Entries are written in the same transaction as the
-- change they record.
CREATE TABLE IF NOT EXISTS formation_activity (
    ulid          TEXT        PRIMARY KEY,                   -- time-ordered; never updated
    formation_uid UUID        NOT NULL REFERENCES formations(uid) ON DELETE CASCADE,
    item_uid      UUID        REFERENCES formation_items(uid) ON DELETE SET NULL,
    actor         TEXT        NOT NULL,                      -- username, or the service identity
    set_by        TEXT        NOT NULL,                      -- user | system
    action        TEXT        NOT NULL,                      -- status_changed | assigned | skipped | ...
    before        JSONB,                                     -- redacted summary
    after         JSONB,
    at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS formation_activity_feed_idx ON formation_activity (formation_uid, ulid DESC);

-- Additive migrations, for a database that already went through an earlier
-- version of this file.
--
-- CREATE TABLE IF NOT EXISTS is a no-op against a table that exists, so a
-- column added to one of the definitions above never reaches a database that
-- has already been created — every read and write of that table then fails on
-- the missing column. Columns therefore have to be added twice: in the
-- definition, for a new database, and here, for an existing one. Each statement
-- stays idempotent for the same reason the rest of the file does.

-- formations.sections carries the section snapshot the checklist serves.
ALTER TABLE formations ADD COLUMN IF NOT EXISTS sections JSONB NOT NULL DEFAULT '[]';

-- Backfilled from the pinned template rather than left at the default: the
-- reader takes sections from this column alone and no longer falls back to the
-- template, so a checklist that predates the column would serve an empty
-- sections[] and render worse than it did before the column existed.
--
-- Only rows that have nothing are touched, which is what makes re-running this
-- safe: a snapshot an upgrade has since extended must not be reset to whatever
-- its creation template says today.
UPDATE formations f
   SET sections = (
           SELECT COALESCE(
                      jsonb_agg(jsonb_build_object('key', s ->> 'key', 'title', s ->> 'title') ORDER BY ord),
                      '[]'::jsonb)
             FROM formation_templates t,
                  jsonb_array_elements(t.sections) WITH ORDINALITY AS e(s, ord)
            WHERE t.uid = f.template_uid
       )
 WHERE f.sections = '[]'::jsonb;

-- formations notification state: one nullable timestamp per one-shot email.
-- NULL means the email has not been sent; non-NULL is a sent marker (at-most-once).
ALTER TABLE formations ADD COLUMN IF NOT EXISTS notified_activating_at   TIMESTAMPTZ;
ALTER TABLE formations ADD COLUMN IF NOT EXISTS notified_reminder_3d_at  TIMESTAMPTZ;
ALTER TABLE formations ADD COLUMN IF NOT EXISTS notified_reminder_overdue_at TIMESTAMPTZ;
