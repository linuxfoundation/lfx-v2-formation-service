// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// The actions an item can offer. Stable identifiers: a browser keys its
// wording on these, and the words it chooses are never sent from here.
const (
	ActionMarkInProgress   = "mark_in_progress"
	ActionMarkDone         = "mark_done"
	ActionMarkBlocked      = "mark_blocked"
	ActionSkip             = "skip"
	ActionBackToNotStarted = "back_to_not_started"
	ActionAssign           = "assign"
	ActionSetDueDate       = "set_due_date"
	ActionSetNote          = "set_note"
	ActionSetEvidenceLink  = "set_evidence_link"
)

// The relations an action can require. Sending them discloses nothing: a caller
// can read the same facts off a 403.
//
// These name the grant a person is given rather than the relation the gateway
// rule checks, which is the `_guard` superset admitting global team grants as
// well. That is on purpose. A browser has no relation list to intersect — its
// model of project capability is a single boolean — so the values have to be
// ones it can evaluate: `auditor` is implied by having read the checklist at
// all, `writer` maps to that boolean, and team membership follows the staff
// persona. Naming the `_guard` relations here would only add a mapping step.
const (
	// RelationAuditor is read access on the project, which is all that
	// leaving an update takes. The assignee does the work off-platform and
	// records what they did; staff read it and move the status.
	RelationAuditor = "auditor"
	// RelationWriter is write access on the project: assignment and due
	// dates, which direct somebody else's work without judging it.
	RelationWriter = "writer"
	// RelationFormationTeam is membership of the formation team. A status
	// change needs write access *and* this, but naming the team alone is
	// accurate rather than lossy: the team reaches writer through the model,
	// so nobody holds the membership without the write access. It is also
	// what keeps an assignee from closing their own item — an assignee
	// elevated to writer in order to do the work still does not hold it.
	RelationFormationTeam = "formation_team_member"
)

// AvailableAction is one thing that may be done to an item in its current
// state, by somebody holding the stated relation.
//
// It carries nothing about any particular caller. The service is not permitted
// to learn what the caller holds — no arrow into it carries a permission — so
// it states what the item permits and which guard each action sits behind, and
// the browser intersects that with standing it already has.
type AvailableAction struct {
	Action           string
	RequiresReason   bool
	RequiresRelation string
}

// statusActions are the actions that move an item from one status to another,
// and they are the architecture review's five controls. Each names its
// destination rather than its legality: whether the move is allowed comes from
// AllowedItemTransitions, so the list and the write path cannot disagree about
// which edges exist.
//
// requiresReason mirrors a refusal the write path already issues rather than
// introducing a rule here. Skipping without a reason is rejected outright;
// blocking or sending back without one leaves the assignee nothing to act on.
//
// All five sit behind the formation team, because all five change a status.
var statusActions = []struct {
	action         string
	to             ItemStatus
	requiresReason bool
}{
	{ActionMarkInProgress, StatusInProgress, false},
	{ActionMarkDone, StatusDone, false},
	{ActionMarkBlocked, StatusBlocked, true},
	{ActionSkip, StatusSkipped, true},
	{ActionBackToNotStarted, StatusNotStarted, true},
}

// fieldActions change a field without touching status, so they do not consult
// the transition table and are offered in every status.
//
// The split between writer and auditor here is the review's, and it is the
// reason publishing this list is worth anything to a partner: an assignee
// holding only read access can leave a note or an evidence link, and nothing
// about the item's status tells a browser that.
var fieldActions = []AvailableAction{
	{Action: ActionAssign, RequiresRelation: RelationWriter},
	{Action: ActionSetDueDate, RequiresRelation: RelationWriter},
	{Action: ActionSetNote, RequiresRelation: RelationAuditor},
	{Action: ActionSetEvidenceLink, RequiresRelation: RelationAuditor},
}

// AvailableActionsFor reports what may be done to an item in this status, given
// the lifecycle of the checklist holding it.
//
// The signature takes no caller, and that absence is the point rather than an
// oversight: a caller argument added here would put the service back in the
// business of resolving permissions, with nothing looking wrong from the
// inside. The caller-independence test exists to catch exactly that.
//
// Returns an empty slice, never nil, so the wire carries [] rather than null: a
// consumer must be able to tell "nothing is permitted" from "the field is
// missing".
func AvailableActionsFor(status ItemStatus, lifecycle Lifecycle) []AvailableAction {
	actions := make([]AvailableAction, 0, len(statusActions)+len(fieldActions))

	// A completed or frozen checklist refuses every change, so offering one
	// would be a lie no matter what the item's own status permits.
	if !lifecycle.Mutable() {
		return actions
	}

	for _, candidate := range statusActions {
		if !status.AllowsTransitionTo(candidate.to) {
			continue
		}
		actions = append(actions, AvailableAction{
			Action:           candidate.action,
			RequiresReason:   candidate.requiresReason,
			RequiresRelation: RelationFormationTeam,
		})
	}

	return append(actions, fieldActions...)
}
