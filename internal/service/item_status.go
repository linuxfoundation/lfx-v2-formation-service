// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"strings"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// statusesNeedingReason are the three transitions that leave somebody with work
// to redo or an explanation owed. Blocking says the work cannot proceed,
// skipping says it will not happen, and sending an item back says what was done
// was not accepted — in each case a bare status change tells the assignee
// nothing about what to do next.
//
// The other two are not in here and are not an oversight: marking something in
// progress or done adds no obligation to anybody.
var statusesNeedingReason = map[model.ItemStatus]string{
	model.StatusBlocked:    reasonBlockedReasonRequired,
	model.StatusSkipped:    reasonSkipReasonRequired,
	model.StatusNotStarted: reasonReturnReasonRequired,
}

// SetItemStatus moves one checklist item to a new status, or sets the status of
// its sub-items, or both.
//
// The narrowest of the three item routes: the gateway guards it on membership
// of the formation team on top of write access. That guard is what stops an
// assignee closing their own item, so the separation is the enforcement, not a
// tidiness preference.
//
// Sub-items are here because a sub-item status is a status, drawn from the same
// enum. On a wider route somebody could march every sub-item to done without
// holding what closing the parent takes.
func (s *Service) SetItemStatus(ctx context.Context, p *svc.SetItemStatusPayload) (*svc.SetItemStatusResult, error) {
	if s.uow == nil {
		// Server misconfiguration, not a client problem: falls through
		// Goa's default formatter as a 500, same rationale as the
		// nil-authenticator branch in JWTAuth.
		return nil, errors.New("no unit of work wired")
	}

	result, _, err := s.mutateItem(ctx, p.ProjectUID, p.ItemKey, p.IfMatch,
		func(item *model.Item) (port.ItemPatch, string, error) {
			patch, err := buildStatusPatch(item, p)
			return patch, statusAction(p), err
		})
	if err != nil {
		return nil, mapItemMutationError(err)
	}

	// After the commit, never inside it. Publishing from within the
	// transaction would ship a state that can still roll back, and would hold
	// the row lock across three network calls.
	s.refreshIndex(ctx, p.ProjectUID)

	// LifecycleLive rather than the formation's stored value: this line is only
	// reached when the write committed, and a write only commits when the
	// read-only check inside the transaction passed. A checklist that was not
	// live could not have produced this result.
	item := itemToWire(result, model.LifecycleLive)
	return &svc.SetItemStatusResult{Item: item, Etag: itemETag(item)}, nil
}

// buildStatusPatch validates the requested transition against the item's
// current state and turns it into a port.ItemPatch. It returns before setting
// anything on the returned patch once it finds a refusal, so a caller never
// sees a partial patch alongside an error.
func buildStatusPatch(item *model.Item, p *svc.SetItemStatusPayload) (port.ItemPatch, error) {
	var patch port.ItemPatch

	// Refused rather than applied as a no-op: Update always increments the
	// revision, so an empty body would invalidate every other client's
	// if_match and append an activity entry recording no change.
	if p.Status == nil && p.SubItems == nil {
		return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonNoFieldsToUpdate)
	}

	if p.Status != nil {
		newStatus := model.ItemStatus(*p.Status)

		// A restated parent status is harmless when real sub-item changes
		// accompany it, but is not itself a mutation.
		if newStatus != item.Status {
			if !item.Status.AllowsTransitionTo(newStatus) {
				return port.ItemPatch{}, domain.NewReasonError(domain.ErrConflict, reasonInvalidTransition)
			}

			reason := ""
			if p.Reason != nil {
				reason = strings.TrimSpace(*p.Reason)
			}
			// Trimmed before the emptiness test, not after it. The
			// skip_needs_reason database constraint compares
			// btrim(skip_reason), so a whitespace-only reason passes a bare
			// == "" test here and then surfaces as a 500 from Postgres
			// rather than as this 400.
			if reasonCode := statusesNeedingReason[newStatus]; reason == "" && reasonCode != "" {
				return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonCode)
			}

			patch.Status = &newStatus

			// Where the reason comes to rest differs by transition, because
			// the two are read back in different places: a skip reason is the
			// standing explanation for why an item was excused and belongs on
			// its own field, while blocking or sending back is a running
			// comment on work still in play. The activity entry records the
			// change either way.
			if reason != "" {
				switch newStatus {
				case model.StatusSkipped:
					patch.SkipReason = &reason
				case model.StatusBlocked, model.StatusNotStarted:
					patch.Note = &reason
				case model.StatusInProgress, model.StatusDone:
					// Neither obliges anybody, so neither keeps a reason.
					// Sent anyway, it is dropped rather than refused: a
					// caller that includes one has not done anything wrong.
				}
			}
		}
	}

	if p.SubItems != nil {
		// Refused rather than skipped. Goa's generated validator steps over
		// nil elements without reporting them, so `"sub_items":[null]`
		// arrives here as a live nil and used to panic the handler on the
		// first field access. Answering 400 also tells the caller their
		// payload was wrong, which quietly dropping the entry would not.
		for _, u := range p.SubItems {
			if u == nil {
				return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonSubItemNull)
			}
		}
		subItems, err := subItemsFromWire(item.SubItems, p.SubItems)
		if err != nil {
			return port.ItemPatch{}, err
		}
		if subItemsChanged(item.SubItems, subItems) {
			patch.SubItems = &subItems
		}
	}

	if patch.Status == nil && patch.SubItems == nil {
		return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonNoFieldsToUpdate)
	}

	return patch, nil
}

func subItemsChanged(before, after []model.SubItem) bool {
	if len(before) != len(after) {
		return true
	}
	for i := range before {
		if before[i] != after[i] {
			return true
		}
	}
	return false
}

// statusAction names the activity entry. The parent's status wins when both
// travel together: it is the more consequential of the two, and sub-items are
// informational — the parent's status is not derived from them.
func statusAction(p *svc.SetItemStatusPayload) string {
	if p.Status != nil {
		return "status_changed"
	}
	return "sub_items_changed"
}
