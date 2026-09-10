// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/service/email"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// Reconciler creates missing checklists and keeps lifecycles in step with their
// projects' stages.
//
// The reconcile is the mechanism of record for creation and repair, not a
// backstop behind change notifications. Notifications arrive on a best-effort
// channel with no replay, so one lost while this service is restarting is lost
// for good — the sweep is what makes the outcome correct regardless.
//
// A project event listener runs alongside it and calls ReconcileProject on the
// project a change names, which is what normally makes a checklist appear within
// seconds. That does not demote the sweep; it changes what the interval means.
// The interval is now the worst case when the listener is not working, rather
// than the ordinary wait, which is why it is a day rather than a quarter of an
// hour. It stays configured rather than compiled in, because lowering it is
// still the knob to reach for during an incident.
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

	// items is used by the notification path to evaluate IsActivating and
	// announcement reminders. Wired separately (not in NewReconciler) so test
	// call-sites that don't exercise notifications do not have to change.
	items port.ItemRepository

	// emailer dispatches one-shot formation notification emails. Nil means
	// email is not configured; all notification paths degrade silently.
	emailer  port.EmailDispatcher
	emailCfg EmailConfig
}

// SetEmailer wires the email dispatcher, item repository, and config into an
// existing Reconciler. Called after NewReconciler so test call-sites that do
// not exercise notifications do not have to change.
func (r *Reconciler) SetEmailer(items port.ItemRepository, emailer port.EmailDispatcher, cfg EmailConfig) {
	r.items = items
	r.emailer = emailer
	r.emailCfg = cfg
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
// replica that has just started may be the only one running, and with the
// interval now a day, a project that entered formation during a deployment would
// otherwise wait until tomorrow.
//
// Every line it logs names the replica that produced it. There is no leader
// election, so a three-replica deployment sweeps three times a day and each
// sweep reads every forming project — a real and accepted cost, but one that
// looks like a bug to anyone reading the logs who does not already know. Naming
// the replica makes three sweeps legible as three replicas rather than as a loop
// firing more often than it was configured to.
func (r *Reconciler) Run(ctx context.Context) {
	replica := replicaName()

	slog.InfoContext(ctx, "reconcile loop started",
		"interval", r.interval, "replica", replica,
		// Stated at startup because the interval alone no longer explains when a
		// checklist appears, and this is the line someone finds when asking why
		// it took a day.
		"note", "the listener accelerates this; the sweep is the backstop")

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		if report, err := r.ReconcileOnce(ctx); err != nil {
			// Logged and dropped rather than returned: the loop must outlive a
			// bad sweep. Whatever failed is still wrong at the next tick, and
			// stopping here would mean nothing ever repairs it.
			slog.ErrorContext(ctx, "reconcile sweep failed", "replica", replica, "error", err)
		} else {
			slog.InfoContext(ctx, "reconcile sweep finished",
				"replica", replica,
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
			slog.InfoContext(ctx, "reconcile loop stopped", "replica", replica)
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
	sweep := &sweepState{
		existing:      existing,
		existingKnown: existingKnown,
		template:      r.prepare(ctx, len(projects)),
	}

	for _, project := range projects {
		report.Swept++
		r.reconcileProject(ctx, project, sweep, report, TriggerSweep)
	}

	return report, nil
}

// replicaName identifies which pod produced a log line.
//
// The hostname, because in Kubernetes that is the pod name and no other
// identifier is both already present and stable for the pod's life. An
// unreadable hostname is not worth failing over — the sweep still runs, it is
// just anonymous.
func replicaName() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown"
	}
	return name
}

// ReconcileProject brings one named project into line with its stage, outside
// any sweep, and reports what it did.
//
// The entry point for anything that already knows which project changed — a
// listener reacting to an event, or an operator naming one. It resolves for this
// single project what a sweep resolves once for all of them, then runs the same
// routine, so a project reconciled this way is indistinguishable from one the
// sweep reached.
//
// It asks after this one project rather than reading the whole checklist set,
// which is the one place it deliberately diverges from the sweep. The full read
// is right for the sweep — it amortizes over every project and supplies the
// project list's request in the same breath — but on the event path it selects
// every project UID in the table to perform a single lookup, and that table only
// grows, because checklists are never deleted. Under a full-catalogue republish
// it would be read once per project in the catalogue.
//
// The two reads still have to agree on what "already has a checklist" means, and
// they do: both answer from the row keyed on project_uid, the column the
// uniqueness constraint is on.
//
// It cannot fail, for the same reason the sweep's per-project routine cannot:
// every outcome is a count. A caller wanting to know whether anything went wrong
// reads Failed on the report.
func (r *Reconciler) ReconcileProject(
	ctx context.Context, project port.ProjectRef, trigger Trigger,
) *ReconcileReport {
	report := &ReconcileReport{}

	existing, existingKnown := r.hasChecklist(ctx, project.UID)
	sweep := &sweepState{
		existing:      existing,
		existingKnown: existingKnown,
		template:      r.prepare(ctx, 1),
	}

	report.Swept++
	r.reconcileProject(ctx, project, sweep, report, trigger)

	return report
}

// reconcileProject brings one project into line with its stage: creating its
// checklist if it needs one and can have one, then finishing it whatever branch
// it took.
//
// Lifted out of the sweep loop so that it is the only place this decision is
// made. A second caller is coming — a listener that reacts to a project event
// rather than waiting for a tick — and the alternative was for that caller to
// re-derive the stage gate, the already-exists check and the template block for
// itself. Two implementations of "what should this project have" would agree on
// the day they were written and drift thereafter, and the one that drifted would
// be the fast path nobody sweeps behind.
//
// It takes the sweep's resolved state rather than resolving its own, because
// resolving per project is exactly what this sweep was changed to stop doing:
// the template read and the checklist-set read cost the same whether they answer
// for one project or sixty. A caller holding one project builds a sweepState for
// it and passes that.
//
// It cannot fail. Every outcome — created, skipped, blocked, failed — is a count
// on the report, because a single project's trouble must not end a sweep that
// still has projects to visit.
func (r *Reconciler) reconcileProject(
	ctx context.Context, project port.ProjectRef, sweep *sweepState, report *ReconcileReport, trigger Trigger,
) {
	// The stage gate. Prospect, Active, Archived and Disengaged create
	// nothing, and an unrecognised value creates nothing either — better a
	// project waits for the next sweep than gets a checklist on a guess.
	//
	// Lifecycle is still synced for those stages: a project that has just
	// become Active or been archived has a checklist to move, which is the
	// case the gate must not skip past.
	if !model.FormingStage(project.SubStage) {
		report.Skipped++
		r.finishProject(ctx, project, report, trigger)
		return
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
		r.finishProject(ctx, project, report, trigger)
		return
	}

	// The first project to get this far is what triggers the template read, and
	// the checks above are what most projects return at — so a sweep or an event
	// that creates nothing reads no templates. Every later project this sweep
	// gets the same answer without a second read.
	//
	// No template was resolvable, which the resolution has already reported
	// once. Lifecycles still need syncing, so the sweep continues rather than
	// returning — but nothing here can be created.
	//
	// Counted as blocked only when the checklist set was actually read.
	// With both reads failing, every project looks absent from an empty
	// set, and counting them all would report the whole sweep as waiting
	// on a template when most of them may need nothing — the same
	// misleading number the check above is ordered to avoid. Degraded is
	// reported instead, and it is a sweep-wide state rather than a per
	// project one, so the resolution's own log already carries the reason.
	template := sweep.template()
	if template == nil {
		if sweep.existingKnown {
			report.Blocked++
		} else {
			report.Degraded++
		}
		r.finishProject(ctx, project, report, trigger)
		return
	}

	created, expandErr := r.expander.ExpandWithTemplate(ctx, project.UID, template, trigger)
	if expandErr != nil {
		// One project's failure must not end the sweep — the rest still
		// need their checklists, and this one is retried next tick. The
		// sweep-wide failure that used to be handled here, a missing
		// template, is now caught before the loop starts.
		report.Failed++
		slog.ErrorContext(ctx, "could not create checklist; continuing the sweep",
			"project_uid", project.UID, "stage", project.SubStage, "error", expandErr)
		return
	}
	if created {
		report.Created++
	}

	// A project can re-enter formation, so a checklist that was frozen or
	// completed has to come back to live. Run after creation because the
	// checklist has to exist before its lifecycle can be moved.
	r.finishProject(ctx, project, report, trigger)
}

// sweepState is what a sweep resolves once and reuses for every project.
type sweepState struct {
	// template resolves the template to create from, at most once per sweep and
	// only when a project actually reaches the point of needing it.
	//
	// It returns nil when nothing can be created: no template is published, or
	// the read failed. Both leave lifecycle sync to run.
	template func() *model.Template
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

// hasChecklist answers knownChecklists' question for one named project, in the
// form reconcileProject reads it.
//
// The same two answers, and the second one carries the same meaning: an empty
// set from a failed read says nothing, where an empty set from a successful one
// says this project needs a checklist. Getting that mapping backwards is the
// hazard here — a transient database error read as "no checklist" would have
// every event attempt a creation, and while the uniqueness constraint absorbs
// those, it would do so a catalogue at a time.
//
// Not found is a successful read. It is how the port says no checklist exists,
// which is exactly the evidence that one should be created.
func (r *Reconciler) hasChecklist(ctx context.Context, projectUID string) (map[string]bool, bool) {
	switch _, err := r.formations.GetByProject(ctx, projectUID); {
	case err == nil:
		return map[string]bool{projectUID: true}, true
	case errors.Is(err, domain.ErrNotFound):
		return map[string]bool{}, true
	default:
		slog.WarnContext(ctx, "could not read whether this project already has a checklist; "+
			"attempting creation and letting the uniqueness constraint absorb a duplicate, "+
			"and reaching no lifecycle that has already left formation",
			"project_uid", projectUID, "error", err)
		return map[string]bool{}, false
	}
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

// prepare arranges the one thing a sweep resolves for every project: where a
// checklist gets created from.
//
// It takes the batch only to say how many projects are waiting when it has to
// report that no template exists. The checklist set it used to be handed and
// pass straight through is now assembled by the caller, which is also where
// both of its halves are read — a parameter that a function only copies into
// its return value reads as though the function had a use for it.
//
// The template read is deferred rather than made here, and gated on a project
// reaching the branch that needs it rather than on any project being at a
// creating stage. Those are different tests, and the difference is the steady
// state: every forming project is at a creating stage and almost none of them
// need creating, so the stage test passed on every sweep and paid for a full
// ListPublished — template bodies included — to answer for projects that all
// returned at the already-exists check. Deferring means a settled sweep reads no
// templates at all, and a settled event path reads none either.
//
// Resolved at most once whatever the batch size, which is what keeps the missing
// template reported once rather than per project: that is the state a freshly
// deployed environment is in until the template is seeded, and it would
// otherwise be logged for every forming project on every tick.
//
// It cannot fail the sweep. A template is not needed to move a lifecycle, so a
// transient error reading templates must not also stop a project that went
// Active from being completed.
func (r *Reconciler) prepare(ctx context.Context, forming int) func() *model.Template {
	return sync.OnceValue(func() *model.Template {
		tpl, err := r.expander.SelectTemplate(ctx)
		if err == nil {
			return tpl
		}
		if errors.Is(err, domain.ErrNotFound) {
			slog.ErrorContext(ctx, "no published template, so no checklist can be created; "+
				"seed one with formation-cli seed. Lifecycles are still synced",
				"forming_projects", forming)
		} else {
			slog.ErrorContext(ctx, "could not read templates, so no checklist can be created "+
				"this sweep; lifecycles are still synced", "error", err)
		}
		// Nil: this sweep syncs lifecycles and creates nothing, and the next
		// one tries again.
		return nil
	})
}

// finishProject runs the three things every project in the sweep needs whatever
// branch it arrived through: its lifecycle brought into step with its stage, the
// platform rows resolved as far as the platform can answer them, and its queue
// row republished.
//
// In that order, and the order is the whole reason these are one function. The
// lifecycle goes first because it is the one step that can take the checklist out
// of scope for the other two: on the sweep where a project reaches Active or is
// archived, syncing first means the platform pass finds a completed or frozen
// checklist and declines it. Running the pass first would advance rows on a
// checklist about to be closed, which rewrites the record of how it got there.
// The projection then goes last because it carries the results of both:
// publishing first would ship the previous values and leave the queue a tick
// behind on exactly the transition someone is watching for.
//
// The projection runs even for a project that created nothing and moved nothing.
// That is what makes an unpublished or lost row repairable by the ordinary loop
// rather than by a tool somebody has to remember exists: the sweep does not know
// which rows are missing from the index, and asking would cost more than
// republishing.
func (r *Reconciler) finishProject(
	ctx context.Context, project port.ProjectRef, report *ReconcileReport, trigger Trigger,
) {
	// The platform pass declines a checklist whose lifecycle has been completed
	// or frozen, which is the whole protection the ordering above buys — and it
	// only holds if the lifecycle actually moved. A failed sync leaves a closing
	// checklist still reading as live, so running the pass anyway would advance
	// rows on it in exactly the sweep this ordering exists to protect. Skipped
	// for this project only; the next sweep retries the sync and the pass with
	// it.
	safe, lifecycleMoved := r.syncLifecycle(ctx, project, report)
	if safe && platformPassApplies(trigger) {
		r.resolvePlatformItems(ctx, project, report)
	}

	// Active email: fired once, immediately after a lifecycle transition to
	// completed. LifecycleForStage knows the target, so the reconciler can
	// infer it from the stage rather than reading the row back.
	if lifecycleMoved {
		if want, _ := model.LifecycleForStage(project.SubStage); want == model.LifecycleCompleted {
			r.dispatchActiveEmails(ctx, project)
		}
	}

	if r.projector == nil {
		return
	}
	published, err := r.projector.Refresh(ctx, project)
	if err != nil {
		// Logged and counted, never propagated. The checklist in Postgres is
		// correct; only the queue's view of it is stale, and the next sweep
		// republishes. Failing the sweep over this would stop lifecycles moving
		// for every project after this one.
		slog.WarnContext(ctx, "could not publish the queue row; the next sweep will retry",
			"project_uid", project.UID, "error", err)
		report.ProjectionFailed++
		return
	}
	// Counted only when a row actually went out. A project the sweep visited
	// that holds no checklist has nothing to publish and is not a queue row.
	if published {
		report.Projected++
	}

	// Activating + announcement reminder emails. These require the formation's
	// own notification state (to fire at most once) and the items + settings
	// (for IsActivating and the announcement date). Only attempted when a
	// projection was just published — meaning this project has a live checklist
	// — and when the emailer is wired.
	if published && r.emailer != nil && r.emailCfg.Enabled {
		r.dispatchProjectNotifications(ctx, project)
	}
}

// platformPassApplies says whether a trigger is one the platform pass can learn
// anything from.
//
// It cannot, on a project event, and the reason is about information rather than
// cost: the pass asks other services whether a repository, a mailing list or a
// committee exists yet, and none of those appear or disappear because a project
// document changed. A project event is published when the project changes, so it
// carries no news about the things being checked — running the pass on receipt
// re-asks the same questions and gets the same answers.
//
// So the pass belongs to the mechanism that looks again on a schedule, which is
// what the sweep is, and to an operator asking for everything now. That is this
// service's existing division applied to one more step: events accelerate what
// they carry information about, and the sweep remains the mechanism of record
// for the rest. A newly created committee is reflected within the day either
// way.
//
// The saving is what makes it worth stating rather than leaving to habit. The
// pass is the only transaction on the per-project path, and its two queries
// resolve nothing at all while no lookup is registered — so under a
// full-catalogue republish it was a transaction per project, arriving as fast as
// NATS delivers, to reach a conclusion the sweep reaches anyway.
//
// Keeping it in the sweep is also what keeps the gap measurable:
// PlatformUnanswerable is the size of the missing-lookup problem, and a count
// only the sweep produces is still a count, where skipping the pass everywhere
// would report zero rows waiting and read as no rows waiting.
func platformPassApplies(trigger Trigger) bool {
	return trigger != TriggerListener
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
//
// It reports whether the lifecycle is in step, so a caller can tell a project
// whose closing state is unknown from one that is simply where it was.
// dispatchActiveEmails fans out the "project is now Active" email to every
// writer and auditor on the project. It is called at most once per transition
// to LifecycleCompleted; subsequent sweeps do not reach this path because
// SyncTo is a no-op once the lifecycle already matches.
//
// A failure reading settings or sending any individual email is logged and
// swallowed — the checklist transition already committed, so blocking on a
// best-effort notification would be the wrong trade.
func (r *Reconciler) dispatchActiveEmails(ctx context.Context, project port.ProjectRef) {
	if r.emailer == nil || !r.emailCfg.Enabled || r.projects == nil {
		return
	}

	settings, err := r.projects.GetSettings(ctx, project.UID)
	if err != nil {
		slog.WarnContext(ctx, "active email: could not read project settings; not sent",
			"project_uid", project.UID, "error", err)
		return
	}

	projectURL := r.emailCfg.AdminBaseURL + "/manage/projects/" + project.Slug

	recipients := make([]string, 0, len(settings.Writers)+len(settings.Auditors))
	recipients = append(recipients, settings.Writers...)
	recipients = append(recipients, settings.Auditors...)
	for _, username := range recipients {
		if username == "" {
			continue
		}
		// Resolve the username to an email address. The roster stores
		// usernames; addresses are carried in the UserEmails map populated
		// from the same settings reply. A username with no email entry is
		// skipped and logged rather than dispatched to an unroutable address.
		to, ok := settings.UserEmails[username]
		if !ok || to == "" {
			slog.WarnContext(ctx, "active email: skipping recipient — no email address on record",
				"project_uid", project.UID, "recipient", username)
			continue
		}
		subj, html, text, renderErr := email.RenderActive(email.ActiveData{
			ProjectName: project.Slug, // name not on ProjectRef; slug used as fallback
			ProjectURL:  projectURL,
		})
		if renderErr != nil {
			slog.WarnContext(ctx, "active email: render failed", "error", renderErr)
			continue
		}
		if sendErr := r.emailer.Send(ctx, port.EmailMessage{
			To:      to,
			Subject: subj,
			HTML:    html,
			Text:    text,
			GroupID: "formation.active." + project.UID,
		}); sendErr != nil {
			slog.WarnContext(ctx, "active email: send failed",
				"project_uid", project.UID, "to", to, "error", sendErr)
		}
	}
}

// dispatchProjectNotifications sends the Activating and announcement-date
// reminder emails when their conditions are met and they have not been sent
// before. It is called only when the project has a live checklist (published=true)
// and the emailer is wired and enabled.
func (r *Reconciler) dispatchProjectNotifications(ctx context.Context, project port.ProjectRef) {
	if r.items == nil || r.projects == nil {
		return
	}
	// Read the formation to check which notifications have already fired.
	formation, err := r.formations.GetByProject(ctx, project.UID)
	if err != nil {
		// ErrNotFound is normal here (project has no checklist yet). Any other
		// error is logged; neither blocks the sweep.
		if !errors.Is(err, domain.ErrNotFound) {
			slog.WarnContext(ctx, "notification check: could not read formation",
				"project_uid", project.UID, "error", err)
		}
		return
	}

	// Fast exit: all three notifications already sent for this formation.
	allSent := formation.NotifiedActivatingAt != nil &&
		formation.NotifiedReminderThreeDayAt != nil &&
		formation.NotifiedReminderOverdueAt != nil
	if allSent {
		return
	}

	// Read items and settings to evaluate conditions.
	items, err := r.items.ListByFormation(ctx, formation.UID)
	if err != nil {
		slog.WarnContext(ctx, "notification check: could not list items",
			"project_uid", project.UID, "error", err)
		return
	}
	settings, err := r.projects.GetSettings(ctx, project.UID)
	if err != nil {
		slog.WarnContext(ctx, "notification check: could not read settings",
			"project_uid", project.UID, "error", err)
		return
	}

	var announcementDate *string
	if settings.AnnouncementDate != nil && *settings.AnnouncementDate != "" {
		announcementDate = settings.AnnouncementDate
	}

	gateTotal, gateOutstanding := gateSummaryFromItems(items)
	activating := isActivating(gateTotal, gateOutstanding, announcementDate)

	adminToolURL := r.emailCfg.AdminBaseURL + "/manage/projects/" + project.Slug + "/checklist"

	// Name is not on ProjectRef; use the slug as a readable placeholder until
	// the project list reply carries the display name (see the TODO in ports.go).
	projectName := project.Slug

	// Activating email.
	if activating && formation.NotifiedActivatingAt == nil {
		r.sendOneShot(ctx, formation, "notified_activating_at", func() (string, string, string, error) {
			return email.RenderActivating(email.ActivatingData{
				ProjectName:      projectName,
				AnnouncementDate: *announcementDate,
				AdminToolURL:     adminToolURL,
			})
		}, r.emailCfg.FormationInbox, "formation.activating."+project.UID)
	}

	if announcementDate == nil {
		return
	}

	// Parse the announcement date to evaluate reminders.
	ad, parseErr := time.Parse("2006-01-02", *announcementDate)
	if parseErr != nil {
		return
	}
	now := time.Now().UTC()
	daysUntil := ad.Sub(now).Hours() / 24

	// 3-day warning: announcement is within 3 days and project is not yet Active.
	if daysUntil <= 3 && daysUntil > 0 && formation.NotifiedReminderThreeDayAt == nil {
		r.sendOneShot(ctx, formation, "notified_reminder_3d_at", func() (string, string, string, error) {
			return email.RenderAnnouncementReminder(email.AnnouncementReminderData{
				ProjectName:      projectName,
				AnnouncementDate: *announcementDate,
				Kind:             email.ReminderThreeDayWarning,
				AdminToolURL:     adminToolURL,
			})
		}, r.emailCfg.FormationInbox, "formation.reminder_3d."+project.UID)
	}

	// Overdue: announcement date has passed and project is still live (not yet Active).
	if daysUntil <= 0 && formation.NotifiedReminderOverdueAt == nil {
		r.sendOneShot(ctx, formation, "notified_reminder_overdue_at", func() (string, string, string, error) {
			return email.RenderAnnouncementReminder(email.AnnouncementReminderData{
				ProjectName:      projectName,
				AnnouncementDate: *announcementDate,
				Kind:             email.ReminderOverdue,
				AdminToolURL:     adminToolURL,
			})
		}, r.emailCfg.FormationInbox, "formation.reminder_overdue."+project.UID)
	}
}

// sendOneShot marks a notification column as sent (at-most-once), then renders
// and dispatches the email. Marking first means a pod restart between mark and
// send drops the send rather than re-sending on the next sweep — the right
// trade for a low-urgency nudge over a best-effort transport.
func (r *Reconciler) sendOneShot(
	ctx context.Context,
	formation *model.Formation,
	column string,
	render func() (subject, html, text string, err error),
	to, groupID string,
) {
	// Mark first (at-most-once). Only the replica whose UPDATE touches a row
	// (acquired=true) proceeds to Send; the loser exits here so a concurrent
	// sweep does not dispatch a duplicate.
	acquired, markErr := r.formations.MarkNotified(ctx, formation.UID, column)
	if markErr != nil {
		slog.WarnContext(ctx, "notification: could not mark; not sending",
			"formation_uid", formation.UID, "column", column, "error", markErr)
		return
	}
	if !acquired {
		return // another replica already sent this notification
	}

	subject, html, text, renderErr := render()
	if renderErr != nil {
		slog.WarnContext(ctx, "notification: render failed",
			"formation_uid", formation.UID, "column", column, "error", renderErr)
		return
	}

	if sendErr := r.emailer.Send(ctx, port.EmailMessage{
		To:      to,
		Subject: subject,
		HTML:    html,
		Text:    text,
		GroupID: groupID,
	}); sendErr != nil {
		slog.WarnContext(ctx, "notification: send failed",
			"formation_uid", formation.UID, "column", column, "to", to, "error", sendErr)
	}
}

// syncLifecycle moves the project's checklist lifecycle to match its stage.
// It returns (safe, moved) where safe = no error occurred (sweep may continue)
// and moved = the lifecycle actually changed this call.
func (r *Reconciler) syncLifecycle(
	ctx context.Context, project port.ProjectRef, report *ReconcileReport,
) (safe, moved bool) {
	var err error
	moved, err = r.lifecycler.SyncTo(ctx, project.UID, project.SubStage)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "could not sync lifecycle; continuing the sweep",
			"project_uid", project.UID, "stage", project.SubStage, "error", err)
		report.Failed++
		return false, false
	case moved:
		report.LifecyclesMoved++
	}
	return true, moved
}
