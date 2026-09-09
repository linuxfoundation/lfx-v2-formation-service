// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// Reconciler creates missing checklists and keeps lifecycles in step with their
// projects' stages.
//
// The reconcile is the mechanism of record for creation and repair, not a
// backstop behind change notifications. Notifications arrive on a best-effort
// channel with no replay, so one lost while this service is restarting is lost
// for good — the sweep is what makes the outcome correct regardless, and the
// interval is the worst-case delay before a project that entered formation gets
// its checklist. That makes it the knob to reach for during an incident, so it
// is configured rather than compiled in.
//
// It is safe to run on every replica at once, with no leader election and no
// singleton job. Two replicas sweeping the same project both try to create the
// same checklist and the uniqueness constraint on project_uid absorbs the loser,
// so concurrent duplication is a no-op rather than a race to reason about. That
// is a deliberate trade: a little redundant work in exchange for having no
// leader to elect, lose, or fail to notice the loss of.
type Reconciler struct {
	projects   port.ProjectReader
	formations port.FormationRepository
	expander   *Expander
	lifecycler *Lifecycler
	projector  *Projector
	platform   *PlatformChecker
	interval   time.Duration
}

// NewReconciler wires a reconciler. A non-positive interval falls back to the
// configured default rather than to a ticker that panics.
// A nil projector disables publishing, so a deployment without NATS still
// creates checklists and moves lifecycles. A nil platform checker likewise skips
// the platform pass.
func NewReconciler(
	projects port.ProjectReader,
	formations port.FormationRepository,
	expander *Expander,
	lifecycler *Lifecycler,
	projector *Projector,
	platform *PlatformChecker,
	interval time.Duration,
) *Reconciler {
	if interval <= 0 {
		interval = constants.DefaultReconcileInterval
	}
	return &Reconciler{
		projects:   projects,
		formations: formations,
		expander:   expander,
		lifecycler: lifecycler,
		projector:  projector,
		platform:   platform,
		interval:   interval,
	}
}

// ReconcileReport is what one sweep did.
type ReconcileReport struct {
	Swept           int
	Created         int
	LifecyclesMoved int
	Skipped         int
	Failed          int

	// Blocked counts projects that should have had a checklist created but
	// could not have one, for a reason that is the same for all of them —
	// today, only the absence of a readable template.
	//
	// Separate from Failed so the summary does not report a hundred failures
	// with no hundred error logs behind them. Blocked says "one condition, this
	// many projects waiting on it", and the condition is logged once.
	Blocked int

	// Projected counts the queue rows republished this sweep, and
	// ProjectionFailed the ones that could not be.
	//
	// A projection failure is counted separately from Failed rather than folded
	// into it, because it means something different to whoever reads the log: a
	// checklist exists and is correct in Postgres, and only the queue's view of
	// it is stale. Nothing is lost and the next sweep republishes it.
	Projected        int
	ProjectionFailed int

	// PlatformResolved counts items a platform check advanced to done this
	// sweep, PlatformUnanswerable the platform rows nobody can answer for, and
	// PlatformCheckFailed the passes that errored.
	//
	// PlatformUnanswerable is the one worth watching while the lookup registry
	// is empty: it is the size of the gap, per sweep, rather than an assertion
	// that a gap exists. Every platform row falls into it today.
	PlatformResolved     int
	PlatformUnanswerable int
	PlatformCheckFailed  int

	// Degraded counts projects the sweep could not reach a conclusion about,
	// because it could not read which checklists already exist and could not
	// resolve a template either. Distinct from Blocked, which asserts the
	// project needs a checklist and cannot have one: with both reads gone the
	// sweep does not know that, and saying so would put a confident number on a
	// guess. Lifecycle sync still runs for these.
	Degraded int
}

// Run sweeps on a ticker until ctx is cancelled.
//
// It sweeps once immediately rather than waiting out the first interval: a
// replica that has just started may be the only one running, and a project that
// entered formation during a deployment should not wait fifteen minutes.
func (r *Reconciler) Run(ctx context.Context) {
	slog.InfoContext(ctx, "reconcile loop started", "interval", r.interval)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		if report, err := r.ReconcileOnce(ctx); err != nil {
			// Logged and dropped rather than returned: the loop must outlive a
			// bad sweep. Whatever failed is still wrong at the next tick, and
			// stopping here would mean nothing ever repairs it.
			slog.ErrorContext(ctx, "reconcile sweep failed", "error", err)
		} else {
			slog.InfoContext(ctx, "reconcile sweep finished",
				"swept", report.Swept,
				"created", report.Created,
				"lifecycles_moved", report.LifecyclesMoved,
				"skipped", report.Skipped,
				"failed", report.Failed,
				"blocked", report.Blocked,
				"degraded", report.Degraded,
				"projected", report.Projected,
				"projection_failed", report.ProjectionFailed,
				"platform_resolved", report.PlatformResolved,
				"platform_unanswerable", report.PlatformUnanswerable,
				"platform_check_failed", report.PlatformCheckFailed,
			)
		}

		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "reconcile loop stopped")
			return
		case <-ticker.C:
		}
	}
}

// ReconcileOnce runs a single sweep. Exported so it can be driven directly by a
// test, and so a sweep can be triggered without waiting for a tick.
func (r *Reconciler) ReconcileOnce(ctx context.Context) (*ReconcileReport, error) {
	report := &ReconcileReport{}

	if r.projects == nil {
		// Nothing can supply the project list yet. Reported rather than
		// silently returning an empty sweep, which would look like "no projects
		// need anything" — indistinguishable from working correctly.
		slog.WarnContext(ctx, "no project reader wired; reconcile swept nothing")
		return report, nil
	}

	// Read before the project list, because it is an input to that request and
	// not only a filter applied to its answer. The list is asked for the
	// formation stages *plus* these projects by name, which is what keeps a
	// checklist visible after its project has gone Active or been archived —
	// the two stages that complete and freeze it, and by definition not
	// formation stages. Without naming them, the projects whose lifecycle still
	// needs moving are precisely the ones missing from the answer.
	//
	// This read is unconditional, where it used to be skipped on any sweep with
	// nothing at a creating stage — the steady state. That is a real cost: one
	// query per tick per replica that a settled deployment previously did not
	// make. It is not avoidable by the same short-circuit, because the answer is
	// what decides which projects the sweep is about, so there is nothing to
	// short-circuit on until it has been read. The template read below is still
	// gated, and that is the more expensive of the two.
	existing, existingKnown := r.knownChecklists(ctx)

	projects, err := r.projects.ListFormingProjects(ctx, keysOf(existing))
	if err != nil {
		return report, err
	}

	// Resolved once for the whole sweep rather than per project. Per project
	// meant a template read and a speculative insert for every project on every
	// tick — including the ones that already had everything they needed, which
	// in a steady state is all of them.
	sweep := r.prepare(ctx, projects, existing, existingKnown)

	for _, project := range projects {
		report.Swept++

		// The stage gate. Prospect, Active, Archived and Disengaged create
		// nothing, and an unrecognised value creates nothing either — better a
		// project waits for the next sweep than gets a checklist on a guess.
		//
		// Lifecycle is still synced for those stages: a project that has just
		// become Active or been archived has a checklist to move, which is the
		// case the gate must not skip past.
		if !model.FormingStage(project.SubStage) {
			report.Skipped++
			r.finishProject(ctx, project, report)
			continue
		}

		// A project that already has a checklist needs no creation attempt.
		// Skipping it is not an optimisation of the uniqueness constraint —
		// that still absorbs concurrent replicas, which is what makes the loop
		// safe. It avoids paying for a transaction and a discarded violation on
		// every project on every tick once the backlog is drained.
		//
		// Checked before the template, so a sweep with no template does not
		// report these as blocked: they are not waiting on one.
		//
		// Lifecycle still runs: an existing checklist is exactly the thing that
		// may need moving.
		if sweep.existing[project.UID] {
			r.finishProject(ctx, project, report)
			continue
		}

		// No template was resolvable, which prepare has already reported once.
		// Lifecycles still need syncing, so the sweep continues rather than
		// returning — but nothing here can be created.
		//
		// Counted as blocked only when the checklist set was actually read.
		// With both sweep-wide reads failing, every project looks absent from
		// an empty set, and counting them all would report the whole sweep as
		// waiting on a template when most of them may need nothing — the same
		// misleading number the check above is ordered to avoid. Degraded is
		// reported instead, and it is a sweep-wide state rather than a per
		// project one, so prepare's own log already carries the reason.
		if sweep.template == nil {
			if sweep.existingKnown {
				report.Blocked++
			} else {
				report.Degraded++
			}
			r.finishProject(ctx, project, report)
			continue
		}

		created, expandErr := r.expander.ExpandWithTemplate(ctx, project.UID, sweep.template)
		if expandErr != nil {
			// One project's failure must not end the sweep — the rest still
			// need their checklists, and this one is retried next tick. The
			// sweep-wide failure that used to be handled here, a missing
			// template, is now caught before the loop starts.
			report.Failed++
			slog.ErrorContext(ctx, "could not create checklist; continuing the sweep",
				"project_uid", project.UID, "stage", project.SubStage, "error", expandErr)
			continue
		}
		if created {
			report.Created++
		}

		// A project can re-enter formation, so a checklist that was frozen or
		// completed has to come back to live. Run after creation because the
		// checklist has to exist before its lifecycle can be moved.
		r.finishProject(ctx, project, report)
	}

	return report, nil
}

// sweepState is what a sweep resolves once and reuses for every project.
type sweepState struct {
	// template is nil when nothing can be created this sweep: either no project
	// in it is at a creating stage, so it was never looked up, or the lookup
	// found nothing published or failed. All three leave lifecycle sync to run.
	template *model.Template
	// existing holds the projects that already have a checklist.
	existing map[string]bool
	// existingKnown says whether existing was actually read. When the read
	// failed it is empty for want of an answer rather than because nothing has
	// a checklist, and the difference matters: absence is only evidence that a
	// project needs creating if the set is known.
	existingKnown bool
}

// knownChecklists reads the projects that already hold a checklist, and reports
// whether the read succeeded.
//
// The two answers are separate because an empty set means two different things.
// Read successfully, it says no checklist exists, and a project's absence from
// it is evidence the project needs one. Unread, it says nothing at all, and
// treating absence as evidence would have the sweep describe every project as
// waiting on something.
//
// It cannot fail the sweep. An unread set costs efficiency rather than
// correctness on the creating side — every forming project is attempted and the
// uniqueness constraint absorbs the ones that already exist, which is what this
// sweep did before the set was read at all. It does cost reach: a project that
// has left formation is only in the list because it is named here, so a failed
// read also means no lifecycle is completed or frozen this tick. That is why the
// warning says so rather than only mentioning duplicates.
func (r *Reconciler) knownChecklists(ctx context.Context) (map[string]bool, bool) {
	existing := map[string]bool{}

	uids, err := r.formations.ListProjectUIDs(ctx)
	if err != nil {
		slog.WarnContext(ctx, "could not list existing checklists; attempting every forming project, "+
			"letting the uniqueness constraint absorb the duplicates, and reaching no project that has "+
			"already left formation this sweep", "error", err)
		return existing, false
	}
	for _, uid := range uids {
		existing[uid] = true
	}
	return existing, true
}

// keysOf returns the map's keys, which is the form the project list request
// takes them in.
func keysOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	return out
}

// prepare resolves the template for the sweep, carrying through the checklist
// set already read.
//
// It does no template read at all when nothing in the sweep is at a creating
// stage, which is the steady state. A missing template is reported here rather
// than per project, because that is the state a freshly deployed environment is
// in until the template is seeded and it would otherwise be logged once for
// every forming project on every tick.
//
// It cannot fail the sweep. A template is not needed to move a lifecycle, so a
// transient error reading templates must not also stop a project that went
// Active from being completed.
func (r *Reconciler) prepare(
	ctx context.Context, projects []port.ProjectRef, existing map[string]bool, existingKnown bool,
) *sweepState {
	state := &sweepState{existing: existing, existingKnown: existingKnown}

	anyCreating := false
	for _, project := range projects {
		if model.FormingStage(project.SubStage) {
			anyCreating = true
			break
		}
	}
	if !anyCreating {
		return state
	}

	tpl, err := r.expander.SelectTemplate(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			slog.ErrorContext(ctx, "no published template, so no checklist can be created; "+
				"seed one with formation-cli seed. Lifecycles are still synced",
				"forming_projects", len(projects))
		} else {
			slog.ErrorContext(ctx, "could not read templates, so no checklist can be created "+
				"this sweep; lifecycles are still synced", "error", err)
		}
		// Left nil: this sweep syncs lifecycles and creates nothing, and the
		// next one tries again.
		return state
	}
	state.template = tpl

	return state
}

// finishProject runs the three things every project in the sweep needs whatever
// branch it arrived through: the platform rows resolved as far as the platform
// can answer them, its lifecycle brought into step with its stage, and its queue
// row republished.
//
// In that order, and the order is the whole reason these are one function. A
// platform check can move a gating item to done, which changes whether the
// checklist is ready — so running it after the lifecycle sync would hold that
// readiness back a full interval. The projection then goes last because it
// carries both: publishing first would ship the previous values and leave the
// queue a tick behind on exactly the transition someone is watching for.
//
// The projection runs even for a project that created nothing and moved nothing.
// That is what makes an unpublished or lost row repairable by the ordinary loop
// rather than by a tool somebody has to remember exists: the sweep does not know
// which rows are missing from the index, and asking would cost more than
// republishing.
func (r *Reconciler) finishProject(ctx context.Context, project port.ProjectRef, report *ReconcileReport) {
	r.resolvePlatformItems(ctx, project, report)
	r.syncLifecycle(ctx, project, report)

	if r.projector == nil {
		return
	}
	if err := r.projector.Refresh(ctx, project); err != nil {
		// Logged and counted, never propagated. The checklist in Postgres is
		// correct; only the queue's view of it is stale, and the next sweep
		// republishes. Failing the sweep over this would stop lifecycles moving
		// for every project after this one.
		slog.WarnContext(ctx, "could not publish the queue row; the next sweep will retry",
			"project_uid", project.UID, "error", err)
		report.ProjectionFailed++
		return
	}
	report.Projected++
}

// resolvePlatformItems runs one platform pass over a project's checklist,
// logging rather than propagating a failure so the sweep continues.
//
// This resolves nothing today, and that is a statement about the platform rather
// than about this call: no owning service answers a project-scoped existence
// lookup, so the registry the checker consults is empty and every platform row
// lands in PlatformUnanswerable. The pass runs anyway, because the sweep is where
// this service establishes correctness — an item whose truth lives in another
// service is only ever going to be caught by something that looks again, and a
// check reachable from nowhere would go on being correct and unused. It also
// makes the gap countable: the sweep reports how many rows are waiting on a
// lookup that does not exist, which is the number to put in front of the teams
// who own those subjects.
func (r *Reconciler) resolvePlatformItems(
	ctx context.Context, project port.ProjectRef, report *ReconcileReport,
) {
	if r.platform == nil {
		return
	}

	pass, err := r.platform.ResolveFor(ctx, project.UID)
	if err != nil {
		slog.WarnContext(ctx, "could not run the platform checks; the next sweep will retry",
			"project_uid", project.UID, "error", err)
		report.PlatformCheckFailed++
		return
	}

	report.PlatformResolved += pass.Advanced
	report.PlatformUnanswerable += pass.Unsupported
	report.PlatformCheckFailed += pass.Failed
}

// syncLifecycle moves one project's lifecycle and records the outcome on the
// report, logging rather than propagating a failure so the sweep continues.
//
// The failure is counted rather than only logged. Reporting the outcome here is
// what makes that possible: returning a bare "did it move" collapsed a failure
// into the same false as a checklist that needed no move, so a sweep that could
// not move a single lifecycle still summarised itself as failed=0.
func (r *Reconciler) syncLifecycle(ctx context.Context, project port.ProjectRef, report *ReconcileReport) {
	moved, err := r.lifecycler.SyncTo(ctx, project.UID, project.SubStage)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "could not sync lifecycle; continuing the sweep",
			"project_uid", project.UID, "stage", project.SubStage, "error", err)
		report.Failed++
	case moved:
		report.LifecyclesMoved++
	}
}
