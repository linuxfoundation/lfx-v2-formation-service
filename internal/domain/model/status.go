// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package model holds the formation domain entities.
package model

// ItemStatus is the stored status of a checklist item. These are stable
// identifiers, never display text: a wording change on the screen must stay a
// display-layer edit rather than a data migration.
type ItemStatus string

// The five statuses, matching the canonical wire contract exactly.
//
// There is deliberately no state between in_progress and done. An earlier
// revision carried awaiting_acceptance, where an assignee claimed the work was
// finished and the formation team confirmed it on a separate route. The
// architecture review rules a five-value enum instead and keeps assignees from
// closing their own items with a guard rather than a state: a status change
// needs write access *and* formation-team membership, which an assignee
// elevated to writer in order to do the work still does not hold.
const (
	StatusNotStarted ItemStatus = "not_started"
	StatusInProgress ItemStatus = "in_progress"
	StatusBlocked    ItemStatus = "blocked"
	StatusDone       ItemStatus = "done"
	StatusSkipped    ItemStatus = "skipped"
)

// AllItemStatuses lists every valid status, in the order the progress strip
// renders them.
var AllItemStatuses = []ItemStatus{
	StatusNotStarted,
	StatusInProgress,
	StatusBlocked,
	StatusDone,
	StatusSkipped,
}

// Valid reports whether s is one of the five known statuses.
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

// AllowedItemTransitions is every status edge a person may make, and it is the
// architecture review's five controls expressed as a graph: mark in progress,
// mark done, mark blocked, skip, and send back to not started.
//
// done is reachable directly, which an earlier revision forbade in order to
// force a claim through a separate acceptance route whose guard was narrower
// than this one's. That safeguard is not dropped, it moves: the gateway now
// applies the formation-team check to any payload carrying a status, so the
// narrow guard sits on the transition itself rather than on a parallel path.
// The two changes are meaningless apart and must ship together.
//
// Every state can go back to not_started, which the stakeholders asked for
// explicitly: a reviewer who decides work was not done correctly has to be able
// to return it, and a derived transition must not then re-advance it.
//
// It lives in the domain rather than beside the route because it is the one
// authority on which moves are legal, and a reader that only describes the
// item — never mutating it — has to consult the same table the write path
// enforces. Two enumerations would drift.
var AllowedItemTransitions = map[ItemStatus][]ItemStatus{
	StatusNotStarted: {StatusInProgress, StatusBlocked, StatusDone, StatusSkipped},
	StatusInProgress: {StatusNotStarted, StatusBlocked, StatusDone, StatusSkipped},
	StatusBlocked:    {StatusNotStarted, StatusInProgress, StatusDone, StatusSkipped},
	StatusDone:       {StatusNotStarted, StatusInProgress},
	StatusSkipped:    {StatusNotStarted, StatusInProgress},
}

// AllowsTransitionTo reports whether an item in this status may move to next.
func (s ItemStatus) AllowsTransitionTo(next ItemStatus) bool {
	for _, allowed := range AllowedItemTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
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
