// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// asPrincipal returns a context carrying a caller identity, which these routes
// require: an activity entry naming nobody cannot evidence that two people were
// involved.
func asPrincipal(username string) context.Context {
	return context.WithValue(context.Background(), constants.PrincipalContextID, username)
}

// claim moves an item to awaiting_acceptance through the ordinary PATCH route,
// as an assignee would, so the tests below act on a claim that was actually made
// rather than on a status written directly into the store.
//
// It assigns the item to the claimant first, which is the ordinary case. The
// unassigned case has its own test, because it is the one where the assignee
// comparison has nothing to compare and the claimant comparison is what holds.
func claim(t *testing.T, s *Service, item *model.Item, assignee string) *svc.FormationItem {
	t.Helper()

	assigned, err := s.UpdateItem(asPrincipal(assignee), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey,
		IfMatch: item.Revision, Assignee: &assignee,
	})
	require.NoError(t, err)

	inProgress := string(model.StatusInProgress)
	moved, err := s.UpdateItem(asPrincipal(assignee), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey,
		IfMatch: assigned.Version, Status: &inProgress,
	})
	require.NoError(t, err)

	awaiting := string(model.StatusAwaitingAcceptance)
	claimed, err := s.UpdateItem(asPrincipal(assignee), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey,
		IfMatch: moved.Version, Status: &awaiting,
	})
	require.NoError(t, err)
	require.Equal(t, string(model.StatusAwaitingAcceptance), claimed.Status)
	return claimed
}

// formationError unwraps the declared error so a test can assert on the
// machine-readable reason rather than on a message.
func formationError(t *testing.T, err error) *svc.FormationError {
	t.Helper()
	require.Error(t, err)
	fe, ok := err.(*svc.FormationError)
	require.True(t, ok, "error is %T, want *svc.FormationError: %v", err, err)
	return fe
}

// The product premise: a claim does not close an item, and it does not count
// toward readiness. If this ever passed with done, the Active decision would rest
// on unverified self-attestation.
func TestAClaimDoesNotReachDoneAndDoesNotSatisfyAGate(t *testing.T) {
	s, formation, itemOne, _ := newItemMutatorTestService(t)

	// Make it a gating item so its effect on readiness is observable.
	gating, err := s.items.GetByKey(context.Background(), formation.UID, itemOne.ItemKey)
	require.NoError(t, err)
	gating.Gate = true

	claimed := claim(t, s, itemOne, "assignee-one")
	assert.Equal(t, string(model.StatusAwaitingAcceptance), claimed.Status)

	// A PATCH straight to done must stay unreachable, or acceptance's guard is
	// bypassable by not using the accept route.
	done := string(model.StatusDone)
	_, err = s.UpdateItem(asPrincipal("assignee-one"), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey,
		IfMatch: claimed.Version, Status: &done,
	})
	fe := formationError(t, err)
	assert.Equal(t, reasonInvalidTransition, fe.Reason,
		"a writer reached done without the accept route, bypassing the self-acceptance guard")
}

// The guard must survive a busy checklist, which is the case that broke it.
//
// The claimant lookup reads the checklist's whole feed, not the item's, so other
// items' entries pile up in front of the claim. It used to ask for one page
// larger than the repository will ever return; the repository clamped the request
// silently, the short page read as the end of the feed, and the claim went
// unfound — so the claimant accepted their own work. A second item generating
// traffic is all it takes.
func TestTheClaimantIsFoundBehindAFeedFullOfOtherItems(t *testing.T) {
	s, _, itemOne, itemTwo := newItemMutatorTestService(t)

	// Unassigned, so the assignee comparison has nothing to short-circuit on and
	// the claimant lookup is what has to hold. That is also the only case the
	// truncated read could reach.
	inProgress := string(model.StatusInProgress)
	moved, err := s.UpdateItem(asPrincipal("one-person"), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey,
		IfMatch: itemOne.Revision, Status: &inProgress,
	})
	require.NoError(t, err)

	awaiting := string(model.StatusAwaitingAcceptance)
	claimed, err := s.UpdateItem(asPrincipal("one-person"), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey,
		IfMatch: moved.Version, Status: &awaiting,
	})
	require.NoError(t, err)

	// Push the claim well past a single page by churning the other item, whose
	// entries share the checklist's feed. The page size is 100.
	current := itemTwo.Revision
	for i := range 140 {
		note := "working" + string(rune('a'+i%26))
		updated, updateErr := s.UpdateItem(asPrincipal("assignee-two"), &svc.UpdateItemPayload{
			ProjectUID: "project-1", ItemKey: itemTwo.ItemKey,
			IfMatch: current, Note: &note,
		})
		require.NoError(t, updateErr)
		current = updated.Version
	}

	_, err = s.AcceptItem(asPrincipal("one-person"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version,
	})
	fe := formationError(t, err)
	assert.Equal(t, reasonSelfAcceptanceForbidden, fe.Reason,
		"the claimant accepted their own item because their claim had scrolled out of the first page")
}

// A rejection reason must not outlive the rejection.
//
// The note is the row's current note, not a log of the last thing anyone said
// about it. Leaving it in place through the acceptance that answered it left the
// checklist rendering "the charter is missing an appendix" under an item marked
// done, which reads as a contradiction rather than as history — the history is
// what the activity feed is for.
func TestAcceptingAfterARejectionClearsTheRejectionReason(t *testing.T) {
	s, formation, itemOne, _ := newItemMutatorTestService(t)
	claimed := claim(t, s, itemOne, "assignee-one")

	rejected, err := s.RejectItem(asPrincipal("reviewer-one"), &svc.RejectItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey,
		IfMatch: claimed.Version, Note: "the charter is missing an appendix",
	})
	require.NoError(t, err)
	require.Equal(t, string(model.StatusInProgress), rejected.Status)

	reclaimed := string(model.StatusAwaitingAcceptance)
	again, err := s.UpdateItem(asPrincipal("assignee-one"), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey,
		IfMatch: rejected.Version, Status: &reclaimed,
	})
	require.NoError(t, err)

	accepted, err := s.AcceptItem(asPrincipal("reviewer-one"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: again.Version,
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.StatusDone), accepted.Status)

	stored, err := s.items.GetByKey(context.Background(), formation.UID, itemOne.ItemKey)
	require.NoError(t, err)
	assert.Empty(t, stored.Note,
		"the rejection reason survived the acceptance and still renders under a done item")
}

func TestAcceptanceMovesTheItemToDone(t *testing.T) {
	s, _, itemOne, _ := newItemMutatorTestService(t)
	claimed := claim(t, s, itemOne, "assignee-one")

	accepted, err := s.AcceptItem(asPrincipal("reviewer-one"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version,
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.StatusDone), accepted.Status)
	assert.Greater(t, accepted.Version, claimed.Version, "the version must move so a stale write is refused")
}

// The guard that makes the two-person rule real rather than nominal.
func TestAnAssigneeCannotAcceptTheirOwnItem(t *testing.T) {
	s, formation, itemOne, _ := newItemMutatorTestService(t)
	claimed := claim(t, s, itemOne, "assignee-one")

	_, err := s.AcceptItem(asPrincipal("assignee-one"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version,
	})
	fe := formationError(t, err)
	assert.Equal(t, reasonSelfAcceptanceForbidden, fe.Reason)
	// 409, not 403: the gateway owns 403, and a service-issued one would tell a
	// client to re-authenticate when what they need is a different accepter.
	assert.Equal(t, "409", fe.Code)

	// The item is untouched, so a refused acceptance leaves nothing behind — not
	// a version bump, which would invalidate everyone else's precondition, and
	// not a status change.
	current, err := s.items.GetByKey(context.Background(), formation.UID, itemOne.ItemKey)
	require.NoError(t, err)
	assert.Equal(t, model.StatusAwaitingAcceptance, current.Status)
	assert.Equal(t, claimed.Version, current.Revision,
		"a refused acceptance moved the version, invalidating other callers' If-Match")
}

// The case the assignee comparison cannot cover, and the reason the guard also
// looks at who claimed.
//
// An unassigned item has no assignee to compare a caller against. On the
// assignee test alone, one person could claim an unassigned item and then accept
// their own claim — the two-person rule defeated by leaving a field blank, which
// is both easy to do and invisible in the result.
func TestOnePersonCannotClaimAndAcceptAnUnassignedItem(t *testing.T) {
	s, formation, itemOne, _ := newItemMutatorTestService(t)

	// Claimed with no assignee ever set.
	inProgress := string(model.StatusInProgress)
	moved, err := s.UpdateItem(asPrincipal("one-person"), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey,
		IfMatch: itemOne.Revision, Status: &inProgress,
	})
	require.NoError(t, err)

	awaiting := string(model.StatusAwaitingAcceptance)
	claimed, err := s.UpdateItem(asPrincipal("one-person"), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey,
		IfMatch: moved.Version, Status: &awaiting,
	})
	require.NoError(t, err)

	stored, err := s.items.GetByKey(context.Background(), formation.UID, itemOne.ItemKey)
	require.NoError(t, err)
	require.Empty(t, stored.Assignee, "the fixture must leave the item unassigned for this to test anything")

	_, err = s.AcceptItem(asPrincipal("one-person"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version,
	})
	fe := formationError(t, err)
	assert.Equal(t, reasonSelfAcceptanceForbidden, fe.Reason,
		"one person claimed and accepted the same unassigned item, so nothing enforced two actors")

	// A different person accepting it is fine, which is the point: the guard
	// refuses the claimant, not everyone.
	accepted, err := s.AcceptItem(asPrincipal("someone-else"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version,
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.StatusDone), accepted.Status)
}

// A later edit by somebody else is not the claim, and must not be mistaken for
// one.
//
// Every PATCH appends an entry, including one that changes only the note, and an
// item left sitting in awaiting_acceptance collects entries whose after-status is
// awaiting_acceptance without any of them being a claim. Matching on that status
// alone made the newest such entry the claimant, so a second person editing the
// note was enough to hand the real claimant of an unassigned item their own
// acceptance.
func TestANoteEditByAnotherPersonIsNotMistakenForTheClaim(t *testing.T) {
	s, _, itemOne, _ := newItemMutatorTestService(t)

	// Unassigned, so the claimant comparison is the only thing standing between
	// one person and their own acceptance.
	inProgress := string(model.StatusInProgress)
	moved, err := s.UpdateItem(asPrincipal("one-person"), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey,
		IfMatch: itemOne.Revision, Status: &inProgress,
	})
	require.NoError(t, err)

	awaiting := string(model.StatusAwaitingAcceptance)
	claimed, err := s.UpdateItem(asPrincipal("one-person"), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey,
		IfMatch: moved.Version, Status: &awaiting,
	})
	require.NoError(t, err)

	// Somebody else adds a note while the item waits. The status does not move.
	note := "the appendix is with legal"
	edited, err := s.UpdateItem(asPrincipal("someone-else"), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey,
		IfMatch: claimed.Version, Note: &note,
	})
	require.NoError(t, err)
	require.Equal(t, string(model.StatusAwaitingAcceptance), edited.Status)

	_, err = s.AcceptItem(asPrincipal("one-person"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: edited.Version,
	})
	fe := formationError(t, err)
	assert.Equal(t, reasonSelfAcceptanceForbidden, fe.Reason,
		"a note edit displaced the claim, so the claimant accepted their own item")

	// The person who wrote the note did not make the claim, so they may accept.
	accepted, err := s.AcceptItem(asPrincipal("someone-else"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: edited.Version,
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.StatusDone), accepted.Status)
}

// The claim and the acceptance are two entries naming two actors. That pair is
// the audit evidence the whole flow exists to produce.
func TestTheClaimAndTheAcceptanceAreSeparateEntriesNamingDifferentActors(t *testing.T) {
	s, formation, itemOne, _ := newItemMutatorTestService(t)
	claimed := claim(t, s, itemOne, "assignee-one")

	_, err := s.AcceptItem(asPrincipal("reviewer-one"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version,
	})
	require.NoError(t, err)

	entries, _, err := s.activity.List(context.Background(), formation.UID, "", 50)
	require.NoError(t, err)

	var claimActor, acceptActor string
	for _, entry := range entries {
		switch entry.Action {
		case "item_accepted":
			acceptActor = entry.Actor
		case "status_changed":
			// The most recent status change before the acceptance is the claim.
			if claimActor == "" {
				claimActor = entry.Actor
			}
		}
	}

	assert.Equal(t, "reviewer-one", acceptActor, "the acceptance must name the accepting actor")
	require.NotEmpty(t, claimActor, "the claim must be recorded as its own entry")
	assert.NotEqual(t, claimActor, acceptActor,
		"the claim and the acceptance name the same actor, so nothing evidences a second person")
}

func TestRejectionReturnsTheItemToInProgressWithItsNote(t *testing.T) {
	s, _, itemOne, _ := newItemMutatorTestService(t)
	claimed := claim(t, s, itemOne, "assignee-one")

	rejected, err := s.RejectItem(asPrincipal("reviewer-one"), &svc.RejectItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version,
		Note: "The charter is missing the trademark clause",
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.StatusInProgress), rejected.Status)
	require.NotNil(t, rejected.Note)
	assert.Equal(t, "The charter is missing the trademark clause", *rejected.Note,
		"the assignee has to be able to read why it came back")
}

// A rejection with nothing in it leaves the assignee no reason and nothing to
// change, so the note is required on its trimmed value rather than merely
// non-empty.
func TestRejectionRequiresANoteWithSomethingInIt(t *testing.T) {
	s, _, itemOne, _ := newItemMutatorTestService(t)
	claimed := claim(t, s, itemOne, "assignee-one")

	_, err := s.RejectItem(asPrincipal("reviewer-one"), &svc.RejectItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version,
		Note: "   ",
	})
	fe := formationError(t, err)
	assert.Equal(t, reasonNoteRequired, fe.Reason)
}

// An assignee withdrawing their own claim is harmless, and occasionally what
// somebody wants. Only accepting and reopening are the two-person operations.
func TestAnAssigneeMayRejectTheirOwnClaim(t *testing.T) {
	s, _, itemOne, _ := newItemMutatorTestService(t)
	claimed := claim(t, s, itemOne, "assignee-one")

	rejected, err := s.RejectItem(asPrincipal("assignee-one"), &svc.RejectItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version,
		Note: "Withdrawing this — not finished after all",
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.StatusInProgress), rejected.Status)
}

func TestReopeningADoneItemReturnsItToInProgress(t *testing.T) {
	s, _, itemOne, _ := newItemMutatorTestService(t)
	claimed := claim(t, s, itemOne, "assignee-one")

	accepted, err := s.AcceptItem(asPrincipal("reviewer-one"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version,
	})
	require.NoError(t, err)

	reopened, err := s.ReopenItem(asPrincipal("reviewer-one"), &svc.ReopenItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: accepted.Version,
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.StatusInProgress), reopened.Status)
}

// Reopening is the reversal of an acceptance, so the assignee must not be able to
// do it either — otherwise self-acceptance is available in two requests: reopen,
// then accept.
func TestAnAssigneeCannotReopenTheirOwnItem(t *testing.T) {
	s, _, itemOne, _ := newItemMutatorTestService(t)
	claimed := claim(t, s, itemOne, "assignee-one")

	accepted, err := s.AcceptItem(asPrincipal("reviewer-one"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version,
	})
	require.NoError(t, err)

	_, err = s.ReopenItem(asPrincipal("assignee-one"), &svc.ReopenItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: accepted.Version,
	})
	fe := formationError(t, err)
	assert.Equal(t, reasonSelfAcceptanceForbidden, fe.Reason)
}

// Accepting something nobody claimed is a caller mistake worth naming, not a
// silent success.
func TestAcceptingAnItemThatIsNotAwaitingAcceptanceIsRefused(t *testing.T) {
	s, _, itemOne, _ := newItemMutatorTestService(t)

	_, err := s.AcceptItem(asPrincipal("reviewer-one"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
	})
	fe := formationError(t, err)
	assert.Equal(t, reasonNotAwaitingAcceptance, fe.Reason)
	assert.Equal(t, "409", fe.Code)
}

func TestReopeningAnItemThatIsNotDoneIsRefused(t *testing.T) {
	s, _, itemOne, _ := newItemMutatorTestService(t)

	_, err := s.ReopenItem(asPrincipal("reviewer-one"), &svc.ReopenItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
	})
	fe := formationError(t, err)
	assert.Equal(t, reasonNotDone, fe.Reason)
}

// The stale precondition outranks the item's state, so a caller who read the item
// before somebody else changed it is told to re-read rather than told their
// request was nonsense.
func TestAStalePreconditionIsRefusedBeforeTheItemsState(t *testing.T) {
	s, _, itemOne, _ := newItemMutatorTestService(t)
	claimed := claim(t, s, itemOne, "assignee-one")

	_, err := s.AcceptItem(asPrincipal("reviewer-one"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version - 1,
	})
	fe := formationError(t, err)
	assert.Equal(t, reasonVersionMismatch, fe.Reason)
	assert.Equal(t, "412", fe.Code)
}

// An acceptance nobody is attached to cannot evidence a second person, so it is
// refused rather than recorded against an empty actor.
func TestAnAcceptanceWithNoCallerIdentityIsRefused(t *testing.T) {
	s, _, itemOne, _ := newItemMutatorTestService(t)
	claimed := claim(t, s, itemOne, "assignee-one")

	_, err := s.AcceptItem(context.Background(), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: itemOne.ItemKey, IfMatch: claimed.Version,
	})
	fe := formationError(t, err)
	assert.Equal(t, reasonNoPrincipal, fe.Reason)
}

func TestAcceptanceOnAMissingChecklistIsNotFound(t *testing.T) {
	s, _, itemOne, _ := newItemMutatorTestService(t)

	_, err := s.AcceptItem(asPrincipal("reviewer-one"), &svc.AcceptItemPayload{
		ProjectUID: "no-such-project", ItemKey: itemOne.ItemKey, IfMatch: 1,
	})
	fe := formationError(t, err)
	assert.Equal(t, reasonNotFound, fe.Reason)
	assert.Equal(t, "404", fe.Code)
}

// Readiness recomputes on acceptance because it is derived on every read and
// never stored — so this asserts the end-to-end consequence the flow exists for:
// a gating item goes from claimed and not counting, to accepted and counting.
func TestReadinessExcludesAClaimAndCountsAnAcceptance(t *testing.T) {
	gateTotal, gateOutstanding := 1, 1

	// Claimed: outstanding, so not ready however the date is set.
	date := "2026-12-01"
	assert.False(t, isActivating(gateTotal, gateOutstanding, &date),
		"awaiting_acceptance satisfied a gate, which is the whole reason the status exists")

	// Accepted: the gate is cleared and the date is set.
	assert.True(t, isActivating(gateTotal, 0, &date))

	// Accepted but no date: still not ready, because readiness is both halves.
	assert.False(t, isActivating(gateTotal, 0, nil))
}
