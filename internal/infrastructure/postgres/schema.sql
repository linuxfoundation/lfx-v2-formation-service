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

-- Project applications. Unrelated to every table above: an application exists
-- before any project does, so it has no project_uid and no formation, and
-- nothing here references or is referenced by a checklist.
--
-- state carries no CHECK constraint. Only 'accepted' and 'denied' are fixed
-- names; what an application is called before a decision, and after a
-- withdraw, is not settled, and a CHECK would freeze a vocabulary nobody has
-- agreed to into the one place that is hardest to change later.
--
-- target_parent_uid is a hint prefilled from wherever the submitter started.
-- It never decides the incorporated entity and never grants anyone anything,
-- so it is nullable and — like project_uid above — an opaque reference with no
-- foreign key.
CREATE TABLE IF NOT EXISTS project_applications (
    uid                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    state              TEXT        NOT NULL,
    revision           BIGINT      NOT NULL DEFAULT 1,

    -- The submitter as data, not as a credential. The UI creates the record as
    -- itself, so the end user never authenticates to this service and nothing
    -- attests these three columns at write time.
    submitter_username TEXT        NOT NULL,
    submitter_name     TEXT        NOT NULL,
    submitter_email    TEXT        NOT NULL,

    target_parent_uid  TEXT,                                 -- opaque; a hint, never a placement

    -- The source names the intake fields but does not define wire keys, types
    -- or requiredness, so the answers remain one document.
    payload            JSONB       NOT NULL DEFAULT '{}',

    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- PII-free markers make a lost application cleanup recoverable without
-- retaining the deleted intake answers or applicant identity.
CREATE TABLE IF NOT EXISTS project_application_deletions (
    uid        UUID        PRIMARY KEY,
    revision   BIGINT      NOT NULL,
    deleted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

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

-- One item's own history, for a feed read narrowed to a single item. Column
-- order is the requirement rather than a detail: formation first so the index
-- stays compatible with the unfiltered read's leading predicate, item second
-- as the equality match, and ulid DESC last so the cursor's ordering is served
-- by the index rather than by a sort node. Without this the narrowed query
-- still walks the formation's whole feed discarding non-matching rows to fill
-- a page -- the consumer's page-and-discard relocated into Postgres, where
-- nobody is measuring it.
CREATE INDEX IF NOT EXISTS formation_activity_item_idx ON formation_activity (formation_uid, item_uid, ulid DESC);

-- Additive migrations, for a database that already went through an earlier
-- version of this file.
--
-- CREATE TABLE IF NOT EXISTS is a no-op against a table that exists, so a
-- column added to one of the definitions above never reaches a database that
-- has already been created — every read and write of that table then fails on
-- the missing column. Columns therefore have to be added twice: in the
-- definition, for a new database, and here, for an existing one. Each statement
-- stays idempotent for the same reason the rest of the file does.

-- project_applications.revision supports databases created before application revisioning.
ALTER TABLE project_applications ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1;

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

-- The repositories and GitHub owner row is manual, not platform-checked.
--
-- No service in the platform owns repositories, so nothing can ever answer
-- that row: it was marked platform in the first template version and has been
-- permanently unanswerable ever since. Template version 2 corrects it for
-- checklists expanded from now on, but a published template is immutable and
-- a live checklist pins the version it expanded from, so every checklist
-- already in flight would keep the old row forever. This is what corrects
-- those.
--
-- Keyed on the item key rather than on the template version, so it reaches
-- those rows whichever version they came from.
--
-- status is deliberately absent from the SET list. This changes who is
-- expected to answer the row, not what the answer is — a row somebody had
-- already marked done or blocked keeps that exactly. revision and updated_at
-- are left alone for the same reason: no caller made this change, and bumping
-- the revision would invalidate reads that are already in flight.
--
-- Live checklists only. A completed or frozen formation is a record of how its
-- project got to Active or to the archive, and the service will not advance one
-- either (internal/service/platform_check.go, which stops on
-- Lifecycle.Mutable()). Correcting who is expected to answer a row is pointless
-- once nothing will ever ask again, so this holds to the same boundary rather
-- than reaching behind it.
UPDATE formation_items
   SET status_source  = 'manual',
       platform_check = NULL
 WHERE item_key       = 'repositories_github_owner'
   AND status_source  = 'platform'
   AND formation_uid IN (SELECT uid FROM formations WHERE lifecycle = 'live');
