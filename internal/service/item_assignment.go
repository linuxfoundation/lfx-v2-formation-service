// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"time"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// AssignItem directs one checklist item's work: who is doing it, and by when.
//
// The middle of the three item routes, on write access. Naming somebody and
// giving them a deadline is not a record of work done, so read access is not
// enough for it; nor does it judge whether the work was done, which is why it
// does not need the formation-team check that moving a status does.
func (s *Service) AssignItem(ctx context.Context, p *svc.AssignItemPayload) (*svc.AssignItemResult, error) {
	if s.uow == nil {
		// Server misconfiguration, not a client problem: falls through
		// Goa's default formatter as a 500, same rationale as the
		// nil-authenticator branch in JWTAuth.
		return nil, errors.New("no unit of work wired")
	}

	// Checked before the transaction opens, because it is a NATS round-trip to
	// another service bounded only by that client's timeout, and inside the
	// transaction it would hold the row lock the later Update takes for the
	// whole trip — the same reason the template upgrade resolves its project
	// facts before opening one.
	//
	// The result is carried, not returned, and reported from inside the patch
	// builder. Returning it here would put this refusal ahead of the
	// missing-checklist, read-only and stale-precondition ones, so a stale
	// write to a frozen checklist would answer 400 where it answers 409 — a
	// change to the wire contract, made by accident, in the name of not
	// holding a lock.
	//
	// The cost is one wasted round-trip when the request was going to be
	// refused on a precondition anyway. That is the right trade against
	// holding a row lock across a call to another service.
	var assigneeErr error
	if p.Assignee != nil && *p.Assignee != "" {
		assigneeErr = validateAssignee(ctx, s.projects, p.ProjectUID, *p.Assignee)
	}

	result, prevAssignee, err := s.mutateItem(ctx, p.ProjectUID, p.ItemKey, p.IfMatch,
		func(_ *model.Item) (port.ItemPatch, string, error) {
			patch, err := buildAssignmentPatch(p, assigneeErr)
			return patch, assignmentAction(p), err
		})
	if err != nil {
		return nil, mapItemMutationError(err)
	}

	// Fire the item-assigned email when the assignee was set or changed by this
	// request. Dispatch is best-effort: a failure is logged and never blocks
	// the write that already succeeded. The email service itself uses NATS core
	// with no redelivery, so there is no retry contract to honour.
	//
	// prevAssignee was captured inside the transaction, so a request repeating
	// the current assignee in order to change the due date does not re-send
	// the notification.
	if p.Assignee != nil && *p.Assignee != "" && result.Assignee != prevAssignee {
		s.dispatchItemAssigned(ctx, p.ProjectUID, result)
	}

	// After the commit, never inside it. Publishing from within the
	// transaction would ship a state that can still roll back, and would hold
	// the row lock across three network calls — the same hazard the assignee
	// check above was moved out of the transaction to avoid.
	s.refreshIndex(ctx, p.ProjectUID)

	// LifecycleLive rather than the formation's stored value: this line is only
	// reached when the write committed, and a write only commits when the
	// read-only check inside the transaction passed. A checklist that was not
	// live could not have produced this result.
	item := itemToWire(result, model.LifecycleLive)
	return &svc.AssignItemResult{Item: item, Etag: itemETag(item)}, nil
}

// buildAssignmentPatch validates this route's two fields and turns them into a
// port.ItemPatch. It returns before setting anything on the returned patch once
// it finds a refusal, so a caller never sees a partial patch alongside an error.
//
// assigneeErr carries the result of the membership check the caller ran before
// opening the transaction, so it is reported from the position it would have
// been checked in.
func buildAssignmentPatch(p *svc.AssignItemPayload, assigneeErr error) (port.ItemPatch, error) {
	var patch port.ItemPatch

	// Refused rather than applied as a no-op: Update always increments the
	// revision, so an empty body would invalidate every other client's
	// if_match and append an activity entry recording no change.
	if p.Assignee == nil && p.DueDate == nil {
		return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonNoFieldsToUpdate)
	}

	if p.Assignee != nil {
		if assigneeErr != nil {
			return port.ItemPatch{}, assigneeErr
		}
		patch.Assignee = p.Assignee
	}

	if p.DueDate != nil {
		if *p.DueDate != "" {
			due, err := time.Parse(dueDateLayout, *p.DueDate)
			// Go's parser accepts a year zero and Postgres has none, so
			// without this the value reached the DATE column and came back as
			// a 500 rather than this 400.
			if err != nil || due.Year() < 1 {
				return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonDueDateInvalid)
			}
		}
		patch.DueDate = p.DueDate
	}

	return patch, nil
}

// assignmentAction names the activity entry after the field the caller led
// with. A request may carry both, but the feed gets exactly one entry per call,
// and the assignee is the more consequential of the two.
func assignmentAction(p *svc.AssignItemPayload) string {
	switch {
	case p.Assignee != nil:
		return "assignee_changed"
	case p.DueDate != nil:
		return "due_date_changed"
	default:
		return "item_updated"
	}
}
