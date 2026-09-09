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

	// Sections is a snapshot of section metadata, not a live join to the
	// pinned template. It starts as every section the template had at
	// creation and only ever grows: the upgrade job appends an entry when it
	// adds an item whose section this checklist has not recorded, and never
	// removes one. Matches how Item.Title is already handled — copied in
	// once so a later template edit cannot change what an existing checklist
	// displays, and so an upgraded item can never carry a section key the
	// response's own sections[] does not list.
	Sections []FormationSection `bun:"sections,type:jsonb,notnull"`

	// Revision is the optimistic-lock counter for this row. It is not
	// surfaced on the wire: versioning is deliberately per item, not per
	// checklist, so two people editing different rows both succeed. Only
	// Item.Revision is served (as version, echoed back as If-Match).
	Revision int64 `bun:"revision,notnull"`

	CreatedAt time.Time `bun:"created_at,nullzero,notnull,default:now()"`
	UpdatedAt time.Time `bun:"updated_at,nullzero,notnull,default:now()"`
}

// FormationSection is one entry in a checklist's section snapshot: enough to
// render a section heading, and nothing an item itself needs. Order is the
// slice order, matching how the template's own sections are ordered.
type FormationSection struct {
	Key   string `json:"key"`
	Title string `json:"title"`
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
	//
	// Defaults to false, unlike RequiresWriter: a template marks the rows it
	// genuinely requires rather than excusing the rest. Agreeing with Go's
	// zero value is also what keeps an explicit false representable here
	// without a pointer, since Bun sends a zero-valued notnull field rather
	// than letting the column default apply.
	IsRequired bool `bun:"is_required,notnull"`
	// ChecklistType says which audience the item is for (internal
	// formation-team work, external project-team work, or both). Display
	// metadata only; it does not filter the response.
	//
	// default:'both' is what makes the column default reachable: with only
	// notnull, Bun sends the empty Go value and the SQL default never
	// applies, persisting a "" that violates the response enum.
	ChecklistType ChecklistType `bun:"checklist_type,notnull,default:'both'"`

	// Mutable by a writer.
	Status ItemStatus `bun:"status,notnull"`
	// nullzero because formation_items_assignee_idx is partial on
	// assignee IS NOT NULL. Without it every row Bun inserts carries '' and
	// lands in the index of assigned work — which is the state the update
	// path already goes out of its way to avoid by clearing to NULL rather
	// than to ''. Insert and update have to agree on what "unassigned" is.
	Assignee     string `bun:"assignee,nullzero"`
	Note         string `bun:"note"`
	SkipReason   string `bun:"skip_reason"`
	EvidenceLink string `bun:"evidence_link"`

	// Set by the service.
	ResolvedRef *ResolvedRef `bun:"resolved_ref,type:jsonb"`
	SubItems    []SubItem    `bun:"sub_items,type:jsonb"`

	Revision int64 `bun:"revision,notnull"`

	CreatedAt time.Time `bun:"created_at,nullzero,notnull,default:now()"`
	UpdatedAt time.Time `bun:"updated_at,nullzero,notnull,default:now()"`
}

// ApplyInsertDefaults fills the values a newly expanded item is allowed to
// leave unset. It lives here rather than in each repository so the Postgres
// and mock implementations cannot drift: a double that returned an empty
// ChecklistType would serve a value the response enum rejects, and would hide
// that from every service-level test.
//
// IsRequired needs nothing — its default is false, which is already the zero
// value.
func (i *Item) ApplyInsertDefaults() {
	if i.Revision == 0 {
		i.Revision = 1
	}
	if i.Status == "" {
		i.Status = StatusNotStarted
	}
	if i.ChecklistType == "" {
		i.ChecklistType = ChecklistBoth
	}
	if i.StatusSource == "" {
		i.StatusSource = SourceManual
	}
	// An empty slice, not nil: the column is NOT NULL DEFAULT '[]', and a nil
	// slice marshals to JSON null rather than []. Done here rather than with a
	// nullzero tag, which would also make an update that clears every sub-item
	// send SQL NULL and violate the constraint.
	if i.SubItems == nil {
		i.SubItems = []SubItem{}
	}
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

// ApplyUpsertDefaults fills what a seeded template may leave unset. Same
// reason ApplyInsertDefaults exists: the state column defaults to 'draft',
// but a notnull field with no default tag sends its zero value, so an unset
// state would persist "" — outside the draft|published|archived vocabulary.
func (t *Template) ApplyUpsertDefaults() {
	if t.State == "" {
		t.State = TemplateDraft
	}
	if t.Sections == nil {
		t.Sections = []TemplateSection{}
	}
	// Published implies a publication time, enforced here rather than trusted
	// from the caller, because the immutability guard is built on it: that guard
	// releases a version whose state is not published *and* whose published_at
	// is nil, so a row that is published with no timestamp reads as never
	// published. Demoting it to draft with identical content then passes the
	// content comparison, and the version's content is open to rewriting from
	// there — the two-step bypass the timestamp exists to close.
	//
	// The seed command does set it, so nothing shipping today produces such a
	// row. That is precisely why it belongs here: the invariant currently holds
	// by the habit of the only caller, and the next writer to publish a template
	// has no reason to know the guard depends on it. Both repositories call this
	// before writing, so this is the one place that covers them.
	if t.State == TemplatePublished && t.PublishedAt == nil {
		now := time.Now().UTC()
		t.PublishedAt = &now
	}
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
	Key       string `json:"key"`
	Title     string `json:"title"`
	OwnerTeam string `json:"owner_team,omitempty"`
	Gate      bool   `json:"gate"`
	// RequiresWriter is a pointer because its default is true: a plain bool
	// cannot tell an authored "false" from an omitted field, and the two mean
	// opposite things here. Resolve it with RequiresWriterOrDefault rather
	// than reading it directly.
	RequiresWriter *bool          `json:"requires_writer,omitempty"`
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

// RequiresWriterOrDefault resolves the authored value, defaulting to true when
// the template omits it: nearly every item is either set by hand or creates
// something on the platform, so acting on it needs Manage. Getting this wrong
// in the permissive direction is the costlier mistake — the value drives the
// prompt that elevates a viewer's access before an item is assigned to them,
// so a false here means that prompt silently never appears.
func (ti *TemplateItem) RequiresWriterOrDefault() bool {
	if ti.RequiresWriter == nil {
		return true
	}
	return *ti.RequiresWriter
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
