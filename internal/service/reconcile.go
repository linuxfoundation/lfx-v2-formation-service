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
	interval   time.Duration
}

// NewReconciler wires a reconciler. A non-positive interval falls back to the
// configured default rather than to a ticker that panics.
func NewReconciler(
	projects port.ProjectReader,
	formations port.FormationRepository,
	expander *Expander,
	lifecycler *Lifecycler,
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

	projects, err := r.projects.ListFormingProjects(ctx)
	if err != nil {
		return report, err
	}

	// Both of these are properties of the sweep, not of a project, so they are
	// resolved once. Resolving them per project meant a template read and a
	// speculative insert for every project on every tick — including the ones
	// that already had everything they needed, which in a steady state is all
	// of them.
	sweep := r.prepare(ctx, projects)

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
			if moved := r.syncLifecycle(ctx, project); moved {
				report.LifecyclesMoved++
			}
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
			if moved := r.syncLifecycle(ctx, project); moved {
				report.LifecyclesMoved++
			}
			continue
		}

		// No template was resolvable, which prepare has already reported once.
		// Lifecycles still need syncing, so the sweep continues rather than
		// returning — but nothing here can be created. Only projects that
		// actually need a checklist reach this, so the count names what is
		// genuinely held up.
		if sweep.template == nil {
			report.Blocked++
			if moved := r.syncLifecycle(ctx, project); moved {
				report.LifecyclesMoved++
			}
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
		if moved := r.syncLifecycle(ctx, project); moved {
			report.LifecyclesMoved++
		}
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
}

// prepare resolves the template and the set of projects that already have a
// checklist, both once for the whole sweep.
//
// It does no work at all when nothing in the sweep is at a creating stage, which
// is the steady state: no template read, no formation listing. A missing
// template is reported here rather than per project, because that is the state a
// freshly deployed environment is in until the template is seeded and it would
// otherwise be logged once for every forming project on every tick.
//
// It cannot fail the sweep. Neither of these lookups is needed to move a
// lifecycle, and one of them is only an optimisation, so a failure here degrades
// what the sweep can do rather than ending it — a transient error reading
// templates must not also stop a project that went Active from being completed.
func (r *Reconciler) prepare(ctx context.Context, projects []port.ProjectRef) *sweepState {
	state := &sweepState{existing: map[string]bool{}}

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

	// Read before the template, and kept even when the template cannot be
	// resolved. The two lookups are independent, and a project that already has
	// a checklist is unaffected by there being no template — counting it as
	// blocked would report a sweep-wide problem against projects that need
	// nothing.
	//
	// The repository exposes this for exactly this diff. Without it the sweep
	// asked the database to refuse an insert once per project per tick and
	// treated the refusal as success.
	uids, err := r.formations.ListProjectUIDs(ctx)
	if err != nil {
		// An empty set is the safe fallback, not a reason to stop: every
		// forming project is then attempted, and the uniqueness constraint
		// absorbs the ones that already exist. That is what this sweep did
		// before the diff existed, so failing to read it costs efficiency
		// rather than correctness.
		slog.WarnContext(ctx, "could not list existing checklists; attempting every forming project "+
			"and letting the uniqueness constraint absorb the duplicates", "error", err)
	}
	for _, uid := range uids {
		state.existing[uid] = true
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

// syncLifecycle moves one project's lifecycle, logging rather than propagating a
// failure so the sweep continues.
func (r *Reconciler) syncLifecycle(ctx context.Context, project port.ProjectRef) bool {
	moved, err := r.lifecycler.SyncTo(ctx, project.UID, project.SubStage)
	if err != nil {
		slog.ErrorContext(ctx, "could not sync lifecycle; continuing the sweep",
			"project_uid", project.UID, "stage", project.SubStage, "error", err)
		return false
	}
	return moved
}
