// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// ReconcileInterval is how often the sweep runs.
//
// The reconcile is the mechanism of record for creation and repair, not a
// backstop behind change notifications. Notifications arrive on a best-effort
// channel with no replay, so one lost while this service is restarting is lost
// for good — the sweep is what makes the outcome correct regardless, and the
// interval is the worst-case delay before a project that entered formation gets
// its checklist.
const ReconcileInterval = 15 * time.Minute

// Reconciler creates missing checklists and keeps lifecycles in step with their
// projects' stages.
//
// It is safe to run on every replica at once, with no leader election and no
// singleton job. Two replicas sweeping the same project both try to create the
// same checklist and the uniqueness constraint on project_uid absorbs the loser,
// so concurrent duplication is a no-op rather than a race to reason about. That
// is a deliberate trade: a little redundant work in exchange for having no
// leader to elect, lose, or fail to notice the loss of.
type Reconciler struct {
	projects   port.ProjectReader
	expander   *Expander
	lifecycler *Lifecycler
	interval   time.Duration
}

// NewReconciler wires a reconciler.
func NewReconciler(projects port.ProjectReader, expander *Expander, lifecycler *Lifecycler) *Reconciler {
	return &Reconciler{
		projects:   projects,
		expander:   expander,
		lifecycler: lifecycler,
		interval:   ReconcileInterval,
	}
}

// ReconcileReport is what one sweep did.
type ReconcileReport struct {
	Swept           int
	Created         int
	LifecyclesMoved int
	Skipped         int
	Failed          int
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

		created, expandErr := r.expander.ExpandFor(ctx, project.UID)
		if expandErr != nil {
			// One project's failure must not end the sweep. The rest still need
			// their checklists, and this one is retried in fifteen minutes.
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
