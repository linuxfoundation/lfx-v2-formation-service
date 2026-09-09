// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// Which template rows the platform can answer for itself, and which it cannot.
//
// The seed template marks three rows status_source: platform — a repository, a
// mailing list, and a committee. None of the three is resolvable today, and the
// reason is the same in each case: no owning service answers "does one of these
// exist for this project".
//
//   - committee. lfx-v2-committee-service answers three request subjects —
//     get_name, list_members and get_project — and every one of them takes a
//     *committee* UID. There is no project-to-committees lookup, so a caller
//     holding only a project UID has no way in.
//   - mailing_list. lfx-v2-mailing-list-service publishes events and subscribes
//     to none of its own: it exposes no request/reply lookup at all.
//   - repository. No service in the platform owns repositories, so there is
//     nothing to ask.
//
// The transport is not an open question, and it is worth saying so here because
// the obvious alternative looks available. Asking query-service for the resource
// type scoped to parent_refs = project:<uid> would be answerable today, but it
// was tried for this exact purpose and failed in production, and the decision
// recorded against this feature is request/reply to the owning service with no
// token on the wire — the plane both donor services already read over, and the
// same one this service reads projects on, Confidential ones included.
//
// That is also why no service identity appears anywhere in this file. A token
// would only be needed to satisfy query-service, which access-checks every hit
// against a user; the internal plane asks for nothing. So the registry is empty
// for want of a subject to send, not for want of permission to send it, and each
// row becomes resolvable when its owning service adds a project-scoped lookup and
// this map gains one entry.
//
// Choosing the owning service also removes a hazard the derived-copy route
// carries, which is worth recording in case it is ever revisited: advancing on an
// index is safe in one direction only. A resource that exists but is not yet
// indexed leaves the row alone for another sweep and corrects itself, while a
// resource deleted before the index catches up would mark a gating row done with
// nothing behind it, and forward-only means nothing walks that back. A service
// answering for its own state cannot be stale about it.
//
// Nothing else here changes when that happens, which is why the unsupported case
// is a first-class outcome rather than an error path: the sweep runs this pass on
// every project already and reports how many rows it could not answer for.
var platformLookups = map[string]platformLookup{
	// Deliberately empty. See above.
}

// platformLookup asks an owning service whether a resource exists for a project.
//
// It returns the reference to the thing it found, so a "Create committee" row can
// become "Open committee" pointing at the real one. A lookup that can only say
// yes or no is not enough for that, which is why this returns a ref rather than a
// bool.
type platformLookup func(ctx context.Context, projectUID string) (*model.ResolvedRef, error)

// errNoLookup reports that no owning service can answer for this resource type.
//
// Distinct from "asked and found nothing". Found-nothing means the item is
// genuinely not done yet and should stay as it is; no-lookup means this service
// cannot tell either way and the row belongs to a person.
var errNoLookup = errors.New("no owning service answers a project-scoped lookup for this resource type")

// PlatformChecker resolves the checklist rows the platform can answer for itself.
type PlatformChecker struct {
	uow     port.UnitOfWork
	lookups map[string]platformLookup
}

// NewPlatformChecker wires a checker over the registered lookups.
func NewPlatformChecker(uow port.UnitOfWork) *PlatformChecker {
	return &PlatformChecker{uow: uow, lookups: platformLookups}
}

// PlatformCheckReport is what one pass over a checklist did.
type PlatformCheckReport struct {
	// Advanced counts items this pass moved to done.
	Advanced int
	// Pending counts platform items that were asked about and are not satisfied
	// yet — the ordinary state of a checklist still being worked.
	Pending int
	// Unsupported counts platform items nobody can answer for, which are left to
	// a person. A non-zero value here is a statement about the platform rather
	// than about this project, so it is reported once per pass and not per item.
	Unsupported int
	// Unchanged counts platform items already done, or in a state a check must
	// not touch.
	Unchanged int
	// Failed counts lookups that errored.
	Failed int
}

// ResolveFor runs every platform check on one project's checklist.
//
// Forward only, and that is the invariant the whole method is arranged around: a
// check may move an item to done and may do nothing else. It never moves an item
// back, never clears a note, and never overwrites a status a person set — so a
// writer who marked a row blocked because the committee was created wrongly does
// not have that judgment silently reversed by a service that can only see that a
// committee exists.
//
// Reaching done here takes no acceptance step, unlike a hand-set item. There is
// nothing for a second person to confirm: the platform is not attesting to its
// own work, it is reporting a fact, and the two-person rule exists to stop
// somebody attesting to theirs.
func (c *PlatformChecker) ResolveFor(ctx context.Context, projectUID string) (*PlatformCheckReport, error) {
	report := &PlatformCheckReport{}
	if c.uow == nil {
		return report, errors.New("no unit of work wired")
	}

	txErr := c.uow.Do(ctx, func(tx port.Tx) error {
		formation, err := tx.Formations().GetByProject(ctx, projectUID)
		if err != nil {
			return err
		}
		// A completed or frozen checklist is not advanced. Its project has gone
		// Active or been archived, and rewriting its rows afterwards would edit
		// the record of how it got there.
		if !formation.Lifecycle.Mutable() {
			return nil
		}

		items, err := tx.Items().ListByFormation(ctx, formation.UID)
		if err != nil {
			return err
		}

		for _, item := range items {
			c.resolveItem(ctx, tx, formation, item, projectUID, report)
		}
		return nil
	})
	if txErr != nil {
		if errors.Is(txErr, domain.ErrNotFound) {
			// No checklist for this project. Not a failure — the reconcile has
			// not created one yet, or the project never needed one.
			return report, nil
		}
		return report, txErr
	}
	return report, nil
}

// resolveItem applies at most one forward move to one item.
func (c *PlatformChecker) resolveItem(
	ctx context.Context,
	tx port.Tx,
	formation *model.Formation,
	item *model.Item,
	projectUID string,
	report *PlatformCheckReport,
) {
	if item.StatusSource != model.SourcePlatform || item.PlatformCheck == nil {
		return
	}

	// Already done, or excused. Nothing a check may do to either: done is where
	// this method's only move leads, and skipped was a person's decision with a
	// reason attached.
	if item.Status == model.StatusDone || item.Status == model.StatusSkipped {
		report.Unchanged++
		return
	}

	lookup, ok := c.lookups[item.PlatformCheck.ResourceType]
	if !ok {
		// Left to a person. Counted rather than logged per item: the reason is
		// the same for every project and every sweep, so a per-item log would say
		// the same sentence thousands of times a day.
		report.Unsupported++
		return
	}

	ref, err := lookup(ctx, projectUID)
	switch {
	case errors.Is(err, errNoLookup):
		report.Unsupported++
		return
	case errors.Is(err, domain.ErrNotFound):
		// Asked, and the thing does not exist yet. The ordinary state of a row
		// still to be done.
		report.Pending++
		return
	case err != nil:
		report.Failed++
		slog.WarnContext(ctx, "a platform check could not be resolved; the item is unchanged",
			"project_uid", projectUID, "item_key", item.ItemKey,
			"resource_type", item.PlatformCheck.ResourceType, "error", err)
		return
	case ref == nil:
		report.Pending++
		return
	}

	done := model.StatusDone
	patch := port.ItemPatch{Status: &done, ResolvedRef: ref}

	updated, err := tx.Items().Update(ctx, item.UID, item.Revision, patch)
	if err != nil {
		// Includes a version mismatch, which here means a person edited the row
		// while this pass was running. Their write wins and this one is dropped
		// rather than retried: the next pass reads the row again, and a check
		// that retried until it won would be the backward move this method is
		// arranged to prevent.
		report.Failed++
		slog.WarnContext(ctx, "a platform check lost a race with a person's edit; leaving their value",
			"project_uid", projectUID, "item_key", item.ItemKey, "error", err)
		return
	}

	// Attributed to the system, not to whoever's request happened to trigger the
	// pass. An entry naming a person for a move they did not make would be worse
	// than no entry at all, because the feed is what the acceptance rule is
	// audited against.
	if err := tx.Activity().Append(ctx, &model.ActivityEntry{
		FormationUID: formation.UID,
		ItemUID:      &updated.UID,
		Actor:        actorSystem,
		SetBy:        model.SetBySystem,
		Action:       "platform_check_resolved",
		Before:       activitySummary(item),
		After:        activitySummary(updated),
	}); err != nil {
		report.Failed++
		slog.WarnContext(ctx, "a platform check advanced an item but could not record it",
			"project_uid", projectUID, "item_key", item.ItemKey, "error", err)
		return
	}

	report.Advanced++
}
