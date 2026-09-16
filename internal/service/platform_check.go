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
// The seeded template marks three rows status_source: platform — a repository,
// a mailing list, and a committee. Two of the three are answerable, and the one
// that is not is not a gap to be closed here:
//
//   - committee and mailing_list are resolved against the shared read layer,
//     which indexes both with the owning project as their parent. That is the
//     one place in the platform where "does this project have one of these"
//     is a single question rather than a per-service lookup that none of the
//     owning services expose: lfx-v2-committee-service answers only by
//     *committee* UID, and lfx-v2-mailing-list-service exposes no
//     request/reply lookup at all.
//   - repository has no owner anywhere in the platform, so there is nothing
//     to ask and no read layer entry to find. That row is manual, and saying
//     so is more honest than leaving it permanently unanswerable.
//
// Reading an index rather than asking each owning service carries one exposure
// worth stating plainly, because it is accepted rather than absent. A resource
// that exists but is not yet indexed leaves the row alone and corrects itself on
// a later sweep, which is harmless. The other direction does not correct itself:
// a resource deleted before the index catches up can advance a row against
// nothing, and forward-only means no later sweep walks that back. The window is
// small, the rows are gates on work that has visibly happened, and a staff
// writer can always set the row by hand — so the cost of being briefly wrong is
// bounded, while the cost of the alternative is that these rows stay
// unanswerable indefinitely.
//
// The registry is supplied by the wiring layer rather than declared here: a
// lookup needs an infrastructure client, and this package must not import
// infrastructure. An environment with no read layer configured gets an empty
// registry and behaves exactly as this service did before — which is why the
// unsupported case is a first-class outcome rather than an error path. The
// sweep runs this pass on every project already and reports how many rows it
// could not answer for.

// PlatformLookup asks the read layer how many of a resource a project has.
//
// Shaped as port.ResourceChecker.Count, and it returns both halves for a reason.
// The count is what the row's min_count is compared against, so a row requiring
// two committees is not satisfied by the first one found. The reference is what
// turns a "Create committee" row into "Open committee" pointing at the real one,
// which a lookup answering only yes or no could not do.
//
// Exported because the wiring layer builds these as closures over an
// infrastructure client, which this package cannot construct itself.
type PlatformLookup func(ctx context.Context, projectUID string) (int, *model.ResolvedRef, error)

// PlatformChecker resolves the checklist rows the platform can answer for itself.
type PlatformChecker struct {
	uow     port.UnitOfWork
	lookups map[string]PlatformLookup
}

// NewPlatformChecker wires a checker over the supplied lookups, keyed by the
// resource_type a template row names. A nil or empty registry is valid and
// means every platform row is reported unanswerable.
func NewPlatformChecker(uow port.UnitOfWork, lookups map[string]PlatformLookup) *PlatformChecker {
	return &PlatformChecker{uow: uow, lookups: lookups}
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
// Reaching done here needs no staff action. The platform is not attesting to
// its own work, it is reporting a fact, and the two-person rule exists to stop
// somebody attesting to theirs.
func (c *PlatformChecker) ResolveFor(ctx context.Context, projectUID string) (*PlatformCheckReport, error) {
	report := &PlatformCheckReport{}
	if c.uow == nil {
		return report, errors.New("no unit of work wired")
	}

	txErr := c.uow.Do(ctx, func(tx port.Tx) error {
		// Locked for the same reason the three write routes lock it: the
		// lifecycle read below is a precondition for writing other rows, and
		// nothing else stops a freeze committing in between. The pass is one
		// project's checklist, so the lock it holds is narrow.
		formation, err := tx.Formations().GetByProjectForUpdate(ctx, projectUID)
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

	count, ref, err := lookup(ctx, projectUID)
	switch {
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
	case ref == nil || count < item.PlatformCheck.MinCount:
		// Found something, but not enough of it. Every row in the seeded template
		// asks for one, so this is the same "not yet" as finding nothing — the
		// distinction only starts to matter for a row asking for two, which the
		// template validation already allows.
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
	// than no entry at all, because the feed is what status changes are audited
	// against.
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
