// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"testing"
)

// allStatuses is every status the enum defines, so a status added without a
// place in these tests fails them rather than slipping through untested.
var allStatuses = []ItemStatus{
	StatusNotStarted,
	StatusInProgress,
	StatusBlocked,
	StatusDone,
	StatusSkipped,
}

// destinations maps a status action back to the status it moves an item to.
// The tests below derive what they expect from AllowedItemTransitions through
// this map rather than hand-listing the answers: a hand-written list would pass
// while agreeing with a transition table that had itself drifted from the write
// path, which is the one failure these tests exist to catch.
var destinations = map[string]ItemStatus{
	ActionMarkInProgress:   StatusInProgress,
	ActionMarkDone:         StatusDone,
	ActionMarkBlocked:      StatusBlocked,
	ActionSkip:             StatusSkipped,
	ActionBackToNotStarted: StatusNotStarted,
}

func TestStatusActionsMatchTheTransitionTable(t *testing.T) {
	for _, status := range allStatuses {
		t.Run(string(status), func(t *testing.T) {
			offered := map[string]bool{}
			for _, action := range AvailableActionsFor(status, LifecycleLive) {
				offered[action.Action] = true
			}

			for action, to := range destinations {
				allowed := status.AllowsTransitionTo(to)
				if offered[action] != allowed {
					t.Errorf("%s from %s: offered=%v, transition table allows=%v",
						action, status, offered[action], allowed)
				}
			}
		})
	}
}

// A status change is the one thing an assignee must not be able to do to their
// own item, so every status action has to name the team guard. An action that
// named only writer would be offered to an assignee elevated to writer in order
// to do the work — the self-closure hole in wire form.
func TestEveryStatusActionRequiresTheFormationTeam(t *testing.T) {
	for _, status := range allStatuses {
		for _, action := range AvailableActionsFor(status, LifecycleLive) {
			if _, isStatusAction := destinations[action.Action]; !isStatusAction {
				continue
			}
			if action.RequiresRelation != RelationFormationTeam {
				t.Errorf("%s from %s requires %q, want %q",
					action.Action, status, action.RequiresRelation, RelationFormationTeam)
			}
		}
	}
}

// Leaving an update is the partner's only action, and it takes read access. If
// this drifts to writer the list stops being worth publishing: a browser would
// hide the note editor from the very person the item is assigned to.
func TestFieldActionsKeepTheirGuards(t *testing.T) {
	want := map[string]string{
		ActionAssign:          RelationWriter,
		ActionSetDueDate:      RelationWriter,
		ActionSetNote:         RelationAuditor,
		ActionSetEvidenceLink: RelationAuditor,
	}

	for _, status := range allStatuses {
		got := map[string]string{}
		for _, action := range AvailableActionsFor(status, LifecycleLive) {
			if _, isStatusAction := destinations[action.Action]; !isStatusAction {
				got[action.Action] = action.RequiresRelation
			}
		}

		if len(got) != len(want) {
			t.Errorf("status %s offers %d field actions, want %d: %v", status, len(got), len(want), got)
		}
		for action, relation := range want {
			if got[action] != relation {
				t.Errorf("status %s: %s requires %q, want %q", status, action, got[action], relation)
			}
		}
	}
}

// Reasons are claimed in the list only where the write path already refuses
// without one. A false negative here sends a browser into a 400 it was told
// would not happen.
func TestReasonsAreClaimedWhereTheWritePathRefusesWithoutOne(t *testing.T) {
	needsReason := map[string]bool{
		ActionMarkBlocked:      true,
		ActionSkip:             true,
		ActionBackToNotStarted: true,
		ActionMarkInProgress:   false,
		ActionMarkDone:         false,
	}

	for _, status := range allStatuses {
		for _, action := range AvailableActionsFor(status, LifecycleLive) {
			want, isStatusAction := needsReason[action.Action]
			if !isStatusAction {
				if action.RequiresReason {
					t.Errorf("field action %s claims to need a reason", action.Action)
				}
				continue
			}
			if action.RequiresReason != want {
				t.Errorf("%s from %s: requiresReason=%v, want %v",
					action.Action, status, action.RequiresReason, want)
			}
		}
	}
}

// A completed or frozen checklist refuses every write, so the list must be
// empty regardless of what the item's own status would otherwise permit.
func TestNonMutableLifecycleOffersNothing(t *testing.T) {
	for _, lifecycle := range []Lifecycle{LifecycleCompleted, LifecycleFrozen} {
		for _, status := range allStatuses {
			if actions := AvailableActionsFor(status, lifecycle); len(actions) != 0 {
				t.Errorf("lifecycle %s, status %s: offered %d actions, want none",
					lifecycle, status, len(actions))
			}
		}
	}
}

// The service is not permitted to learn what a caller holds, so the list cannot
// vary by caller. There is no caller parameter to vary, which is what makes the
// property hold — this asserts the signature stays that way, so that adding one
// breaks a test rather than quietly reintroducing permission resolution here.
func TestActionsDoNotVaryByCaller(t *testing.T) {
	for _, status := range allStatuses {
		first := AvailableActionsFor(status, LifecycleLive)
		for range 5 {
			repeat := AvailableActionsFor(status, LifecycleLive)
			if len(repeat) != len(first) {
				t.Fatalf("status %s: repeated calls disagree (%d vs %d)", status, len(repeat), len(first))
			}
			for i := range first {
				if repeat[i] != first[i] {
					t.Fatalf("status %s, action %d: %+v != %+v", status, i, repeat[i], first[i])
				}
			}
		}
	}
}

// An empty list has to reach the wire as [] rather than null: a consumer must
// be able to tell "nothing is permitted" from "this build does not send the
// field". make([]T, 0) marshals to [] and a nil slice does not, so this pins
// the constructor in AvailableActionsFor.
func TestEmptyListMarshalsAsArrayNotNull(t *testing.T) {
	empty := AvailableActionsFor(StatusDone, LifecycleCompleted)

	encoded, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != "[]" {
		t.Errorf("empty list marshalled as %s, want []", encoded)
	}
}
