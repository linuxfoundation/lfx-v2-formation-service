// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// ItemWriteRefresher republishes a project's indexed documents after one of its
// items has been written.
//
// Declared as an interface so a write route can be tested for having asked for
// a refresh without standing up a projector, a project reader and a publisher
// behind it. The routes depend on this rather than on *Refresher for that
// reason alone; there is one implementation.
//
// There is no error return, and that is the contract rather than an omission.
// This is called after the response has been decided, so there is nobody left
// to act on a failure — see Refresher.AfterItemWrite.
type ItemWriteRefresher interface {
	AfterItemWrite(ctx context.Context, projectUID string)
}

// Refresher publishes a project's queue rows as soon as one of its items is
// written, rather than leaving them to the next sweep.
//
// It is an accelerator in the same shape the project event listener already is:
// everything it does, the sweep does on its own, so a refresh lost to a
// restart, a timeout or an unreachable index costs freshness and never
// correctness. What it buys is the difference between an assignment appearing
// in seconds and appearing within a day, which is the interval the sweep now
// runs at.
//
// It deliberately holds no policy. What to publish and what a document contains
// are decided by the projector, which the sweep also uses — so a document
// published from here is byte-identical to one published by a sweep, and a
// reader cannot tell which path produced it. This type decides only *when*.
//
// It equally deliberately does not run the rest of the sweep. Lifecycles are
// not advanced and platform checks are not re-asked, because an item write
// carries no news about either: a project's stage did not change because
// somebody set a due date, and no repository or mailing list came into
// existence because an item was assigned. Those belong to the mechanism that
// looks again on a schedule.
type Refresher struct {
	projects  port.ProjectReader
	projector *Projector

	// wg tracks refreshes in flight so shutdown can wait for them. They are
	// started by HTTP requests and read Postgres, which is what fixes where the
	// drain has to happen: after the server has stopped accepting requests, and
	// before the pool those requests' refreshes are reading from is released.
	wg sync.WaitGroup

	// mu guards stopped against the Add it is paired with. WaitGroup forbids an
	// Add that races a Wait once the count has reached zero, and shutdown is
	// exactly that race — so the decision to accept work and the Add recording
	// it have to be one atomic step.
	mu      sync.Mutex
	stopped bool

	// abandoned is what every refresh ties its own context to, so the ones
	// still running when the drain budget expires can be cut short as a group
	// rather than tracked one by one.
	//
	// Without this the budget bounds only this type's own waiting. The
	// refreshes would carry on holding Postgres connections, and closing the
	// pool waits for those to come back — so the wait the drain was meant to
	// cap would reappear during teardown, where the pod's grace period is all
	// that is left to absorb it.
	abandoned context.Context
	abandon   context.CancelFunc

	// drainTimeout is how long Stop waits before abandoning. A field rather
	// than the constant read inline so a test can exercise the expiry without
	// spending the production budget waiting for it.
	drainTimeout time.Duration

	// Counted rather than only logged, for the reason the listener's counters
	// exist: with the sweep repairing everything within a day, a refresher that
	// has been failing for a week looks exactly like a week in which nobody
	// assigned anything. Only the counts tell the two apart.
	requested atomic.Int64
	published atomic.Int64
	skipped   atomic.Int64
	failed    atomic.Int64
	dropped   atomic.Int64
}

// Compile-time check that the concrete refresher satisfies what the write
// routes depend on. Stated explicitly because the routes hold the interface: a
// signature drifting out of line would otherwise surface as a write that
// silently stopped refreshing.
var _ ItemWriteRefresher = (*Refresher)(nil)

// NewRefresher wires a refresher onto the projector the sweep already uses.
//
// The projector is shared rather than built here, so there is one answer to
// what a published document looks like. A nil projector or a nil project reader
// disables refreshing, which is how a deployment without NATS still serves its
// write routes — the same posture every other optional dependency in this
// service takes.
func NewRefresher(projects port.ProjectReader, projector *Projector) *Refresher {
	abandoned, abandon := context.WithCancel(context.Background())
	return &Refresher{
		projects:     projects,
		projector:    projector,
		abandoned:    abandoned,
		abandon:      abandon,
		drainTimeout: constants.DefaultRefreshDrainTimeout,
	}
}

// AfterItemWrite starts a refresh for the project and returns immediately.
//
// It never blocks and never fails, and both are requirements rather than
// conveniences. The caller has already committed the item and is about to tell
// somebody their change was saved, which is true whatever happens here: the
// database is the source of truth and the index is derived from it. Failing or
// delaying that response because a search index is unreachable would trade a
// correct write for a fresher document, which is the wrong way round.
//
// The refresh runs under a context detached from the caller's. Goa cancels the
// request context once the response is written, so a goroutine holding it
// directly would find it cancelled before it had done anything — the refresher
// would look wired and would silently never publish. Detaching keeps the trace
// and the request's values while dropping the cancellation, and the deadline
// comes from this service instead.
func (r *Refresher) AfterItemWrite(ctx context.Context, projectUID string) {
	if r == nil || r.projector == nil || r.projects == nil || projectUID == "" {
		return
	}

	r.requested.Add(1)

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		// Counted rather than dropped in silence. A refusal nobody records is
		// indistinguishable from a refresh that never happened, and this one
		// has a reason worth knowing at shutdown.
		r.dropped.Add(1)
		slog.DebugContext(ctx, "not refreshing the queue rows; the service is shutting down",
			"project_uid", projectUID)
		return
	}
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		// net/http recovers a panic raised on the goroutine serving a request:
		// it fails that one request and the process lives. Moving this work off
		// that goroutine leaves it outside that protection, so the same panic
		// that used to cost one request would take the pod down — and it would
		// be reachable by anyone able to write an item, over and over.
		//
		// Recovered here rather than in Projector.Refresh, because the sweep
		// calls that too and is a different question: a panic there happens on
		// a schedule nobody triggers, and crashing is a reasonable answer to it.
		defer func() {
			if recovered := recover(); recovered != nil {
				r.failed.Add(1)
				slog.ErrorContext(ctx, "panic while refreshing the queue rows after an item write",
					"project_uid", projectUID, "panic", recovered,
					"stack", string(debug.Stack()))
			}
		}()

		refreshCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), constants.DefaultRefreshTimeout)
		defer cancel()

		// Detached from the request but not from the process: shutdown can
		// still cut this short once it has waited as long as it agreed to.
		defer context.AfterFunc(r.abandoned, cancel)()

		r.refresh(refreshCtx, projectUID)
	}()
}

// refresh republishes one project's documents and records what happened.
//
// Every outcome is a count, because there is no caller to return to. The three
// it distinguishes mean different things to whoever reads the summary: a
// publish, a project there was nothing to publish for, and a failure that left
// the index stale until the next sweep.
func (r *Refresher) refresh(ctx context.Context, projectUID string) {
	ref, err := r.projects.GetRef(ctx, projectUID)
	if err != nil {
		// A project that no longer exists is not a failure. A checklist
		// outlives its project — nothing deletes one — so a write can land on a
		// project that has since gone, and there is no document to refresh
		// rather than a refresh that went wrong.
		if errors.Is(err, domain.ErrNotFound) {
			r.skipped.Add(1)
			slog.DebugContext(ctx, "no project to refresh the queue rows for",
				"project_uid", projectUID)
			return
		}
		r.failed.Add(1)
		slog.WarnContext(ctx, "could not resolve the project after an item write; "+
			"its queue rows stay stale until the next sweep",
			"project_uid", projectUID, "error", err)
		return
	}

	published, err := r.projector.Refresh(ctx, ref)
	if err != nil {
		// Logged and counted, never propagated — the same treatment the sweep
		// gives a failed projection, and for the same reason: the checklist in
		// Postgres is correct and only the queue's view of it is stale.
		r.failed.Add(1)
		slog.WarnContext(ctx, "could not publish the queue rows after an item write; "+
			"the next sweep will retry", "project_uid", projectUID, "error", err)
		return
	}
	if !published {
		// The project holds no checklist, so there was nothing to publish. Kept
		// apart from a failure for the reason the sweep keeps them apart: a
		// failure nobody can find a cause for is worse than no number at all.
		r.skipped.Add(1)
		return
	}
	r.published.Add(1)
}

// Stop refuses new refreshes and waits for the ones already running.
//
// Bounded, following the listener's reasoning: an unbounded wait lets one
// refresh stuck on an unreachable index spend the pod's whole grace period, and
// the kill then lands somewhere that costs more. What is lost by giving up is a
// document that stays stale until the next sweep, which is the state the
// service was in before any of this existed.
//
// When the budget runs out the remaining refreshes are cancelled rather than
// left running, and this still does not return until they have exited. Letting
// them run would make the budget bound nothing: they hold Postgres connections,
// the caller closes the pool immediately after this returns, and that close
// blocks until every connection comes back — so the wait would simply reappear
// during teardown, past the point where anything is left to bound it.
//
// Cancelling costs the same thing timing out costs: a document stale until the
// next sweep, which is the state the service was in before any of this existed.
func (r *Refresher) Stop(ctx context.Context) {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		r.wg.Wait()
	}()

	timeout := time.NewTimer(r.drainTimeout)
	defer timeout.Stop()
	select {
	case <-drained:
	case <-timeout.C:
		slog.WarnContext(ctx, "cancelling in-flight index refreshes; "+
			"the next sweep republishes whatever they did not",
			"waited", r.drainTimeout)
		r.abandon()
		// Unbounded in form only: every refresh is now running on a cancelled
		// context, so each is one failed NATS or Postgres call from returning.
		<-drained
	}

	// Logged here rather than where the interval summary stops, because only
	// here are the totals final.
	r.logCounts(ctx, "write-path index refresher stopped", "process", r.Counts())
}

// RefreshCounts is what the refresher has done.
type RefreshCounts struct {
	Requested int64
	Published int64
	Skipped   int64
	Failed    int64
	Dropped   int64
}

// since returns what happened between an earlier reading and this one.
func (c RefreshCounts) since(earlier RefreshCounts) RefreshCounts {
	return RefreshCounts{
		Requested: c.Requested - earlier.Requested,
		Published: c.Published - earlier.Published,
		Skipped:   c.Skipped - earlier.Skipped,
		Failed:    c.Failed - earlier.Failed,
		Dropped:   c.Dropped - earlier.Dropped,
	}
}

// Counts reports the refresher's totals.
//
// Requested less the other four is the number in flight, which is what makes a
// refresher stuck on an unreachable index legible rather than merely quiet.
func (r *Refresher) Counts() RefreshCounts {
	if r == nil {
		return RefreshCounts{}
	}
	return RefreshCounts{
		Requested: r.requested.Load(),
		Published: r.published.Load(),
		Skipped:   r.skipped.Load(),
		Failed:    r.failed.Load(),
		Dropped:   r.dropped.Load(),
	}
}

// ReportEvery logs what the refresher did in each interval, until ctx is
// cancelled.
//
// Started on the sweep's interval so each daily sweep summary is accompanied by
// this one and the two can be read against each other. A sweep republishing
// rows next to a refresher that requested nothing all day is the signature of a
// write path that has stopped calling this.
//
// Each tick reports the interval rather than the totals, and is emitted even
// when every count is zero. That zero line is the point: per-event logs can say
// what happened but not that nothing happened, and "nothing happened" is what a
// broken refresher and a quiet day have in common.
func (r *Refresher) ReportEvery(ctx context.Context, interval time.Duration) {
	if r == nil {
		return
	}
	// NewTicker panics on a non-positive interval, and this runs in a goroutine
	// where that takes the process with it. RECONCILE_INTERVAL=0s parses
	// cleanly, so the config layer hands it over unchanged.
	if interval <= 0 {
		interval = constants.DefaultReconcileInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	previous := r.Counts()

	for {
		select {
		case <-ctx.Done():
			// The closing summary belongs to Stop, which knows when the last
			// refresh actually finished.
			return
		case <-ticker.C:
			current := r.Counts()
			r.logCounts(ctx, "write-path index refresher summary",
				interval.String(), current.since(previous))
			previous = current
		}
	}
}

// logCounts emits counts under msg, naming the window they cover.
//
// One function for both the periodic summary and the closing one, because the
// only reason to emit both is that they are comparable, and two copies of the
// same five keys stay comparable exactly as long as nobody edits one of them.
func (r *Refresher) logCounts(
	ctx context.Context, msg, window string, counts RefreshCounts,
) {
	slog.InfoContext(ctx, msg,
		"window", window,
		"requested", counts.Requested,
		"published", counts.Published,
		"skipped", counts.Skipped,
		"failed", counts.Failed,
		"dropped", counts.Dropped)
}
