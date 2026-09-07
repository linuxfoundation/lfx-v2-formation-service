// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// Formation is one project's checklist. There is at most one per project,
// enforced by a uniqueness constraint on project_uid rather than by any
// reservation key, which is what makes the reconcile loop safe to run on every
// replica.
//
// No project facts are stored here. The project's name, slug, sub-stage,
// announcement date and people are owned by other services and read fresh,
// because a copy is a second source of truth that drifts.
type Formation struct {
	bun.BaseModel `bun:"table:formations,alias:f"`

	UID        uuid.UUID `bun:"uid,pk,default:gen_random_uuid()"`
	ProjectUID string    `bun:"project_uid,notnull"`

	// TemplateUID and TemplateVersion are pinned at creation and never
	// revisited, so a template published later cannot change a checklist
	// already in flight.
	TemplateUID     uuid.UUID `bun:"template_uid,notnull"`
	TemplateVersion int       `bun:"template_version,notnull"`

	Lifecycle   Lifecycle  `bun:"lifecycle,notnull"`
	StartedAt   time.Time  `bun:"started_at,nullzero,notnull,default:now()"`
	CompletedAt *time.Time `bun:"completed_at"`

	// Revision is the optimistic-lock counter, surfaced on the wire as
	// version and as an ETag.
	Revision int64 `bun:"revision,notnull"`

	CreatedAt time.Time `bun:"created_at,nullzero,notnull,default:now()"`
	UpdatedAt time.Time `bun:"updated_at,nullzero,notnull,default:now()"`
}

// Item is one checklist row. Revision is per row, not per formation, so
// concurrent owners editing different items never contend on one ETag.
type Item struct {
	bun.BaseModel `bun:"table:formation_items,alias:i"`

	UID          uuid.UUID `bun:"uid,pk,default:gen_random_uuid()"`
	FormationUID uuid.UUID `bun:"formation_uid,notnull"`

	// ItemKey is stable across template versions, which is what makes the
	// upgrade job idempotent and what the browser keys its labels on.
	ItemKey    string `bun:"item_key,notnull"`
	SectionKey string `bun:"section_key,notnull"`
	Position   int    `bun:"position,notnull"`

	// Copied from the template at expansion and immutable thereafter.
	Title          string         `bun:"title,notnull"`
	OwnerTeam      string         `bun:"owner_team"`
	Gate           bool           `bun:"gate,notnull"`
	RequiresWriter bool           `bun:"requires_writer,notnull"`
	StatusSource   StatusSource   `bun:"status_source,notnull"`
	PlatformCheck  *PlatformCheck `bun:"platform_check,type:jsonb"`
	ActionLink     string         `bun:"action_link"`
	DueDate        *time.Time     `bun:"due_date,type:date"`

	// IsRequired says whether this item must be filled in at all. It is
	// metadata for the checklist screen, distinct from Gate: Gate is
	// specifically "blocks Active", IsRequired is not tied to any
	// particular lifecycle transition.
	IsRequired bool `bun:"is_required,notnull"`
	// ChecklistType says which audience the item is for (internal
	// formation-team work, external project-team work, or both). Display
	// metadata only; it does not filter the response.
	ChecklistType ChecklistType `bun:"checklist_type,notnull"`

	// Mutable by a writer.
	Status       ItemStatus `bun:"status,notnull"`
	Assignee     string     `bun:"assignee"`
	Note         string     `bun:"note"`
	SkipReason   string     `bun:"skip_reason"`
	EvidenceLink string     `bun:"evidence_link"`

	// Set by the service.
	ResolvedRef *ResolvedRef `bun:"resolved_ref,type:jsonb"`
	SubItems    []SubItem    `bun:"sub_items,type:jsonb"`

	Revision int64 `bun:"revision,notnull"`

	CreatedAt time.Time `bun:"created_at,nullzero,notnull,default:now()"`
	UpdatedAt time.Time `bun:"updated_at,nullzero,notnull,default:now()"`
}

// SubItem is informational detail embedded in its parent item. The parent's
// status is never derived from these: "2 of 4" is a display string.
type SubItem struct {
	Key    string     `json:"key"`
	Title  string     `json:"title"`
	Status ItemStatus `json:"status"`
}

// PlatformCheck describes how a platform-sourced item is resolved. It exists
// so an item's mechanism can change without changing the contract.
type PlatformCheck struct {
	ResourceType string `json:"resource_type"`
	MinCount     int    `json:"min_count"`
}

// ResolvedRef is what a platform check found, which is what lets "Create
// committee" become "Open committee" once one exists.
type ResolvedRef struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

// Template is a versioned checklist definition. Selection picks the lowest
// priority among published rows whose match rule fires first.
type Template struct {
	bun.BaseModel `bun:"table:formation_templates,alias:t"`

	UID     uuid.UUID     `bun:"uid,pk,default:gen_random_uuid()"`
	Name    string        `bun:"name,notnull"`
	Version int           `bun:"version,notnull"`
	State   TemplateState `bun:"state,notnull"`

	// Priority orders candidates, lower winning. Match is enum-valued
	// rather than an expression language; "always" is the fallback.
	Priority int    `bun:"priority,notnull"`
	Match    string `bun:"match,notnull"`

	Sections []TemplateSection `bun:"sections,type:jsonb,notnull"`

	Author      string     `bun:"author"`
	CreatedAt   time.Time  `bun:"created_at,nullzero,notnull,default:now()"`
	UpdatedAt   time.Time  `bun:"updated_at,nullzero,notnull,default:now()"`
	PublishedAt *time.Time `bun:"published_at"`
}

// TemplateSection groups template items under one heading. The shape is
// nested rather than flat because the checklist screen renders section
// headers with their own metadata lines.
type TemplateSection struct {
	Key   string         `json:"key"`
	Title string         `json:"title"`
	Items []TemplateItem `json:"items"`
}

// TemplateItem is the definition an Item is expanded from.
type TemplateItem struct {
	Key            string         `json:"key"`
	Title          string         `json:"title"`
	OwnerTeam      string         `json:"owner_team,omitempty"`
	Gate           bool           `json:"gate"`
	RequiresWriter bool           `json:"requires_writer"`
	StatusSource   StatusSource   `json:"status_source"`
	IsRequired     bool           `json:"is_required"`
	ChecklistType  ChecklistType  `json:"checklist_type,omitempty"`
	PlatformCheck  *PlatformCheck `json:"platform_check,omitempty"`
	// ActionLink may contain a {{project.uid}} placeholder, substituted
	// once at expansion.
	ActionLink string `json:"action_link,omitempty"`
	// DueRule computes DueDate at expansion, relative to the project's
	// announcement date.
	DueRule  string            `json:"due_rule,omitempty"`
	SubItems []TemplateSubItem `json:"sub_items,omitempty"`
}

// TemplateSubItem is the definition a SubItem is expanded from.
type TemplateSubItem struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	OwnerTeam string `json:"owner_team,omitempty"`
}

// ActivityEntry is one immutable record in the checklist's feed. Rows are
// never updated, so there is no revision. Entries are written in the same
// transaction as the change they record.
type ActivityEntry struct {
	bun.BaseModel `bun:"table:formation_activity,alias:a"`

	// ULID is the primary key and the paging cursor: time-ordered, so the
	// feed and its cursor come from one index read.
	ULID         string     `bun:"ulid,pk"`
	FormationUID uuid.UUID  `bun:"formation_uid,notnull"`
	ItemUID      *uuid.UUID `bun:"item_uid"`

	Actor  string `bun:"actor,notnull"`
	SetBy  SetBy  `bun:"set_by,notnull"`
	Action string `bun:"action,notnull"`

	// Before and After hold redacted summaries, not whole rows.
	Before map[string]any `bun:"before,type:jsonb"`
	After  map[string]any `bun:"after,type:jsonb"`

	At time.Time `bun:"at,nullzero,notnull,default:now()"`
}
