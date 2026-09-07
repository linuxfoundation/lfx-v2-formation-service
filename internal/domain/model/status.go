// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package models holds the formation domain entities.
package model

// ItemStatus is the stored status of a checklist item. These are stable
// identifiers, never display text: a wording change on the screen must stay a
// display-layer edit rather than a data migration.
type ItemStatus string

// The six statuses, matching the canonical wire contract exactly.
const (
	StatusNotStarted ItemStatus = "not_started"
	StatusInProgress ItemStatus = "in_progress"
	StatusBlocked    ItemStatus = "blocked"
	// StatusAwaitingAcceptance sits between in_progress and done: the
	// assignee has claimed the work is finished but the formation team has
	// not confirmed it. It must never count toward readiness.
	StatusAwaitingAcceptance ItemStatus = "awaiting_acceptance"
	StatusDone               ItemStatus = "done"
	StatusSkipped            ItemStatus = "skipped"
)

// AllItemStatuses lists every valid status, in the order the progress strip
// renders them.
var AllItemStatuses = []ItemStatus{
	StatusNotStarted,
	StatusInProgress,
	StatusBlocked,
	StatusAwaitingAcceptance,
	StatusDone,
	StatusSkipped,
}

// Valid reports whether s is one of the six known statuses.
func (s ItemStatus) Valid() bool {
	for _, known := range AllItemStatuses {
		if s == known {
			return true
		}
	}
	return false
}

// SatisfiesGate reports whether an item in this status clears a gating
// requirement. Only done does. A gating item may be skipped, but skipping it
// leaves the project short of readiness rather than waving it through.
func (s ItemStatus) SatisfiesGate() bool {
	return s == StatusDone
}

// Lifecycle is the formation's own lifecycle, distinct from the project's
// sub-stage, which this service reads and never stores.
type Lifecycle string

const (
	// LifecycleLive accepts mutations.
	LifecycleLive Lifecycle = "live"
	// LifecycleCompleted is reached when the project becomes Active.
	LifecycleCompleted Lifecycle = "completed"
	// LifecycleFrozen refuses mutations, for archived or disengaged
	// projects. The checklist is never deleted, so re-entry restores it.
	LifecycleFrozen Lifecycle = "frozen"
)

// Mutable reports whether a checklist in this lifecycle accepts changes.
func (l Lifecycle) Mutable() bool { return l == LifecycleLive }

// TemplateState is the publication state of a template version.
type TemplateState string

const (
	TemplateDraft     TemplateState = "draft"
	TemplatePublished TemplateState = "published"
	TemplateArchived  TemplateState = "archived"
)

// StatusSource says whether an item's status is set by hand or driven by a
// platform check.
type StatusSource string

const (
	SourceManual   StatusSource = "manual"
	SourcePlatform StatusSource = "platform"
)

// SetBy distinguishes a status a person set from one the service derived, so
// the activity feed can tell them apart.
type SetBy string

const (
	SetByUser   SetBy = "user"
	SetBySystem SetBy = "system"
)

// ChecklistType says which audience an item is for. It is display metadata
// only — it does not filter what a caller can read or change what counts
// toward progress or readiness; that is a separate question left for later
// if the product needs it.
type ChecklistType string

const (
	ChecklistInternal ChecklistType = "internal"
	ChecklistExternal ChecklistType = "external"
	ChecklistBoth     ChecklistType = "both"
)
