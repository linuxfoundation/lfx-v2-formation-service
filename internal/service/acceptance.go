// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// The reasons these three routes refuse with, beyond the ones the PATCH route
// already declares and shares.
const (
	// reasonSelfAcceptanceForbidden is the one guard on these routes that lives
	// in the service rather than at the gateway.
	//
	// It is not an authorization check, which is why it is here at all: the
	// gateway can ask whether the caller is on the formation team, but it cannot
	// compare the caller to a value stored on the row being written. So this is
	// domain logic over data that happens to have an authorization-shaped
	// consequence, and it is the reason the two-person rule holds in practice
	// rather than only on paper.
	//
	// Answers 409, not 403. A 403 is what the gateway returns when the caller
	// holds nothing on the project, and a service-issued one would be
	// indistinguishable from it — telling a client to re-authenticate when what
	// they need is a different person to accept.
	reasonSelfAcceptanceForbidden = "self_acceptance_forbidden"

	// reasonNotAwaitingAcceptance covers accept and reject: both act on a claim,
	// and there is no claim to act on unless the item is awaiting acceptance.
	reasonNotAwaitingAcceptance = "not_awaiting_acceptance"

	// reasonNotDone covers reopen, which reverses an acceptance and so needs an
	// acceptance to reverse.
	reasonNotDone = "not_done"

	// reasonNoteRequired is rejection's own: a rejection with no reason leaves
	// the assignee nothing to act on.
	reasonNoteRequired = "note_required"

	// reasonNoPrincipal fires when no caller identity reached the service.
	//
	// Refused rather than recorded as an empty actor. These entries are the
	// evidence that two different people were involved, and an entry naming
	// nobody cannot support that — an unattributed acceptance beside an
	// unattributed claim looks like two distinct actors while proving neither.
	reasonNoPrincipal = "no_principal"
)

// acceptanceReasonMessages gives the reasons above their sentences. Kept
// separate from reasonMessages so a change to one route's vocabulary cannot
// silently alter the other's.
var acceptanceReasonMessages = map[string]string{
	reasonSelfAcceptanceForbidden: "an assignee cannot accept their own item; " +
		"someone else on the formation team has to",
	reasonNotAwaitingAcceptance: "the item is not awaiting acceptance, so there is no claim to act on",
	reasonNotDone:               "only a done item can be reopened",
	reasonNoteRequired:          "a note is required when rejecting, so the assignee knows what to change",
	reasonNoPrincipal:           "the request carries no caller identity, so the decision cannot be attributed",
}

// acceptanceOutcome is what one of the three routes does to an item, expressed
// so the three share a single transaction, guard order and activity shape.
//
// The three differ in exactly four ways — the status they require, the status
// they move to, whether a note is mandatory, and the activity action they record
// — and everything else about them has to be identical: the same precondition
// order, the same optimistic lock, the same audit entry in the same transaction.
// Writing them as three functions meant four opportunities per route for one of
// those to drift.
type acceptanceOutcome struct {
	// from is the status the item must currently be in.
	from model.ItemStatus
	// to is where it goes.
	to model.ItemStatus
	// notRequiredReason is the refusal when from does not match.
	wrongStatusReason string
	// requireNote makes the note mandatory, which only rejection does.
	requireNote bool
	// action names the activity entry.
	action string
	// checkSelf applies the self-acceptance refusal.
	//
	// True for accept and reopen, false for reject. An assignee rejecting their
	// own claim is withdrawing it, which is harmless and occasionally what
	// somebody wants; an assignee accepting or reopening their own is the thing
	// the two-person rule exists to stop. Reopen is included because reopening
	// then re-accepting is the same self-acceptance taking two requests.
	checkSelf bool
}

// AcceptItem moves an item from awaiting_acceptance to done.
//
// The formation-team membership check happens at the gateway, before this runs.
// What is left here is the comparison the gateway cannot make: whether the caller
// is the item's own assignee.
func (s *Service) AcceptItem(ctx context.Context, p *svc.AcceptItemPayload) (*svc.FormationItem, error) {
	return s.applyAcceptance(ctx, acceptanceRequest{
		projectUID: p.ProjectUID,
		itemKey:    p.ItemKey,
		ifMatch:    p.IfMatch,
		note:       p.Note,
	}, acceptanceOutcome{
		from:              model.StatusAwaitingAcceptance,
		to:                model.StatusDone,
		wrongStatusReason: reasonNotAwaitingAcceptance,
		action:            "item_accepted",
		checkSelf:         true,
	})
}

// RejectItem returns a claimed item to in_progress with a note the assignee can
// read.
func (s *Service) RejectItem(ctx context.Context, p *svc.RejectItemPayload) (*svc.FormationItem, error) {
	return s.applyAcceptance(ctx, acceptanceRequest{
		projectUID: p.ProjectUID,
		itemKey:    p.ItemKey,
		ifMatch:    p.IfMatch,
		note:       &p.Note,
	}, acceptanceOutcome{
		from:              model.StatusAwaitingAcceptance,
		to:                model.StatusInProgress,
		wrongStatusReason: reasonNotAwaitingAcceptance,
		requireNote:       true,
		action:            "item_rejected",
		// Deliberately false — see acceptanceOutcome.checkSelf.
		checkSelf: false,
	})
}

// ReopenItem returns a done item to in_progress.
func (s *Service) ReopenItem(ctx context.Context, p *svc.ReopenItemPayload) (*svc.FormationItem, error) {
	return s.applyAcceptance(ctx, acceptanceRequest{
		projectUID: p.ProjectUID,
		itemKey:    p.ItemKey,
		ifMatch:    p.IfMatch,
		note:       p.Note,
	}, acceptanceOutcome{
		from:              model.StatusDone,
		to:                model.StatusInProgress,
		wrongStatusReason: reasonNotDone,
		action:            "item_reopened",
		checkSelf:         true,
	})
}

// acceptanceRequest is the shared payload the three routes reduce to.
type acceptanceRequest struct {
	projectUID string
	itemKey    string
	ifMatch    int64
	note       *string
}

// applyAcceptance runs the whole read-guard-write-log sequence in one
// transaction, so an item that changed status without an activity entry naming
// who changed it is not a state this can reach. That matters more here than on
// the ordinary PATCH: these entries are the evidence that two different people
// were involved, and a missing one is indistinguishable from a self-acceptance.
//
// The guard order is fixed and load-bearing. A caller must not be able to tell,
// from which refusal they get, anything they could not already see: the
// checklist's existence and its read-only state come first, then the
// precondition, then the item's own state, and the self-acceptance refusal comes
// last because it is the only one that discloses who the assignee is.
func (s *Service) applyAcceptance(
	ctx context.Context, req acceptanceRequest, outcome acceptanceOutcome,
) (*svc.FormationItem, error) {
	if s.uow == nil {
		return nil, errors.New("no unit of work wired")
	}

	principal, _ := ctx.Value(constants.PrincipalContextID).(string)
	if principal == "" {
		return nil, mapAcceptanceError(
			domain.NewReasonError(domain.ErrInvalidRequest, reasonNoPrincipal))
	}

	note := ""
	if req.note != nil {
		note = strings.TrimSpace(*req.note)
	}
	// Checked on the trimmed value, because a note of spaces is a note the
	// assignee cannot read. Goa's MinLength(1) rejects only the empty string.
	if outcome.requireNote && note == "" {
		return nil, mapAcceptanceError(
			domain.NewReasonError(domain.ErrInvalidRequest, reasonNoteRequired))
	}

	var result *model.Item
	txErr := s.uow.Do(ctx, func(tx port.Tx) error {
		formation, err := tx.Formations().GetByProject(ctx, req.projectUID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return domain.NewReasonError(domain.ErrNotFound, reasonNotFound)
			}
			return err
		}
		if !formation.Lifecycle.Mutable() {
			return domain.NewReasonError(domain.ErrConflict, reasonChecklistReadOnly)
		}

		item, err := tx.Items().GetByKey(ctx, formation.UID, req.itemKey)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return domain.NewReasonErrorf(domain.ErrNotFound, reasonUnknownItemKey,
					"no item with key %q exists", req.itemKey)
			}
			return err
		}
		if item.Revision != req.ifMatch {
			return domain.NewReasonError(domain.ErrVersionMismatch, reasonVersionMismatch)
		}
		if item.Status != outcome.from {
			return domain.NewReasonError(domain.ErrConflict, outcome.wrongStatusReason)
		}

		// Last, because it is the only refusal here that tells the caller
		// something about the row rather than about their own request — namely
		// that they are its assignee. Everyone who can reach this route is on
		// the formation team and can read the item anyway, so the disclosure is
		// nil in practice; the ordering is so that it stays nil if the gateway
		// rule is ever loosened.
		if outcome.checkSelf {
			blocked, err := s.isSelfAcceptance(ctx, tx, formation.UID, item, principal)
			if err != nil {
				return err
			}
			if blocked {
				return domain.NewReasonError(domain.ErrConflict, reasonSelfAcceptanceForbidden)
			}
		}

		patch := port.ItemPatch{Status: &outcome.to}
		if note != "" {
			patch.Note = &note
		}

		updated, err := tx.Items().Update(ctx, item.UID, item.Revision, patch)
		if err != nil {
			if errors.Is(err, domain.ErrVersionMismatch) {
				return domain.NewReasonError(domain.ErrVersionMismatch, reasonVersionMismatch)
			}
			if errors.Is(err, domain.ErrNotFound) {
				return domain.NewReasonError(domain.ErrNotFound, reasonUnknownItemKey)
			}
			return err
		}

		// A separate entry from the claim it acts on, naming a different actor.
		// That pair is the audit record the two-person rule rests on, and it is
		// why this is one transaction with the status change rather than a
		// best-effort log afterwards.
		if err := tx.Activity().Append(ctx, &model.ActivityEntry{
			FormationUID: formation.UID,
			ItemUID:      &updated.UID,
			Actor:        principal,
			SetBy:        model.SetByUser,
			Action:       outcome.action,
			Before:       activitySummary(item),
			After:        activitySummary(updated),
		}); err != nil {
			return err
		}

		result = updated
		return nil
	})
	if txErr != nil {
		return nil, mapAcceptanceError(txErr)
	}
	return itemToWire(result), nil
}

// claimSearchDepth bounds how far back the claimant lookup reads.
//
// The claim it is looking for is the entry that moved this item to
// awaiting_acceptance, which on any real checklist is within the last handful of
// entries for that item — but the feed is per checklist, not per item, so a busy
// checklist interleaves other items' entries in front of it. This is generous
// enough to cross that interleaving and bounded so the guard cannot turn into a
// full scan of a long-lived checklist's history.
const claimSearchDepth = 200

// isSelfAcceptance reports whether principal may not accept or reopen this item
// because they are the person whose work is being confirmed.
//
// Two comparisons, not one. The assignee is the obvious one and the one the
// requirement names. The claimant is the one that makes the rule true: an item
// with no assignee has nothing to compare against, so on the assignee test alone
// a single person could claim an unassigned item and then accept their own claim
// — which is precisely the "second, different person" the flow exists to
// guarantee, defeated by leaving a field blank.
//
// An unreadable history fails closed. If the claim cannot be read, this cannot
// establish that a second person is accepting, and the whole value of the guard
// is that it is not best-effort.
func (s *Service) isSelfAcceptance(
	ctx context.Context, tx port.Tx, formationUID uuid.UUID, item *model.Item, principal string,
) (bool, error) {
	if item.Assignee != "" && item.Assignee == principal {
		return true, nil
	}

	entries, _, err := tx.Activity().List(ctx, formationUID, "", claimSearchDepth)
	if err != nil {
		return false, err
	}

	// Newest first, so the first matching entry is the claim in force.
	for _, entry := range entries {
		if entry.ItemUID == nil || *entry.ItemUID != item.UID {
			continue
		}
		if !isClaimEntry(entry) {
			continue
		}
		return entry.Actor == principal, nil
	}

	// No claim found within the window. Not refused: the item's status already
	// had to be awaiting_acceptance to reach here, and an item can arrive there
	// through a path this service did not record — a data migration, or a claim
	// older than the window on a checklist with a long history. Refusing would
	// make those items permanently unacceptable by anyone, which is worse than
	// falling back to the assignee comparison already made above.
	return false, nil
}

// isClaimEntry reports whether an activity entry is the completion claim: a
// status change whose result was awaiting_acceptance.
//
// Reads the recorded after-summary rather than trusting the action name, because
// the claim travels as an ordinary status_changed on the shared PATCH route and
// has no action of its own.
func isClaimEntry(entry *model.ActivityEntry) bool {
	status, ok := entry.After["status"].(string)
	return ok && model.ItemStatus(status) == model.StatusAwaitingAcceptance
}

// mapAcceptanceError turns a domain.ReasonError into the declared FormationError,
// preferring this route's own wording for a reason both vocabularies name.
//
// The two maps stay apart deliberately: a refusal these routes phrase in terms of
// acceptance should not have that phrasing follow the reason onto the PATCH route,
// where it would describe a step the caller did not take.
func mapAcceptanceError(err error) error {
	return mapReasonError(err, acceptanceReasonMessages, reasonMessages)
}
