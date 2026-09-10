// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// ProjectListener reacts to published project changes by reconciling the
// project they name, so a checklist appears in seconds rather than at the next
// sweep.
//
// It is an accelerator and nothing else. Every outcome it produces, the sweep
// produces on its own, which is what makes the design tolerable given what the
// transport does not offer: no acknowledgement, no redelivery, no replay, and
// nothing delivered at all while this service is restarting. A lost event costs
// freshness and never a checklist.
//
// It deliberately holds no reconcile logic. Deciding whether a project should
// have a checklist happens in exactly one place, and this is not it — the
// listener converts an event into a ProjectRef and hands it over. Anything that
// looks like a policy decision creeping in here belongs in the reconcile
// instead, because the sweep has to make the same decision and only one of the
// two can be right.
type ProjectListener struct {
	reconciler *Reconciler
	decode     func([]byte) (port.ProjectRef, error)

	// Counted rather than merely logged, because the question this answers is
	// operational rather than forensic: with the sweep running daily, a
	// listener that died weeks ago looks exactly like a quiet week. Only the
	// counts tell the two apart.
	received     atomic.Int64
	handled      atomic.Int64
	droppedStage atomic.Int64
	droppedOther atomic.Int64
	failed       atomic.Int64
}

// NewProjectListener wires a listener onto the reconciler it delegates to.
//
// The decoder is injected rather than imported so this package stays clear of
// the transport: the wire format of another service's event is an infrastructure
// concern, and the day it changes should be a change in one adapter rather than
// in the use case.
func NewProjectListener(
	reconciler *Reconciler, decode func([]byte) (port.ProjectRef, error),
) *ProjectListener {
	return &ProjectListener{reconciler: reconciler, decode: decode}
}

// Start subscribes to each subject and returns a function that stops them all.
//
// A failure to subscribe is returned rather than swallowed, but the caller is
// expected to carry on: the service is correct without this, and refusing to
// start because the accelerator could not attach would turn a latency feature
// into an availability dependency.
//
// The returned stop function is safe to call once. It stops every subscription
// that was established, in the order they were made, and only then cancels the
// context the handlers run under.
//
// That ordering is the reason handlers do not run under ctx directly. Shutdown
// cancels ctx before it drains anything, so a handler holding ctx would find it
// already cancelled and abandon its database transaction — draining would wait
// politely for work that had just been told to give up, which is the opposite of
// what draining is for. Handlers get a context that survives that cancellation
// and is cancelled here instead, once nothing is left in flight. That context is
// what Subscribe is given, so each message's own context — which also carries
// the publisher's trace — descends from it.
func (l *ProjectListener) Start(
	ctx context.Context, subscriber port.Subscriber, queue string, subjects ...string,
) (func(), error) {
	handlerCtx, cancelHandlers := context.WithCancel(context.WithoutCancel(ctx))

	stops := make([]func(), 0, len(subjects))
	stopAll := func() {
		// Bounded, because each drain waits on a handler that may itself be
		// waiting on a NATS request, and this runs before the HTTP server's own
		// shutdown budget rather than sharing it. Unbounded, one wedged handler
		// spends the pod's whole grace period and the kill lands during the
		// HTTP drain instead — trading a lost event, which costs nothing here,
		// for an aborted request, which does.
		//
		// Started together rather than in turn so the budget bounds the slowest
		// drain instead of their sum: sequentially, one wedged handler spends
		// the whole ten seconds and the subscriptions behind it are cancelled
		// without ever being asked to drain.
		var wg sync.WaitGroup
		for _, stop := range stops {
			wg.Add(1)
			go func() {
				defer wg.Done()
				stop()
			}()
		}

		drained := make(chan struct{})
		go func() {
			defer close(drained)
			wg.Wait()
		}()

		timeout := time.NewTimer(constants.DefaultListenerDrainTimeout)
		defer timeout.Stop()
		select {
		case <-drained:
		case <-timeout.C:
			slog.WarnContext(ctx, "gave up waiting for event handlers to finish; cancelling them",
				"waited", constants.DefaultListenerDrainTimeout)
		}

		cancelHandlers()

		// Logged here rather than where the ticker notices the cancellation,
		// because only here are the totals final: at cancellation there may
		// still be a handler mid-transaction, and a summary printed then would
		// undercount the very work the drain exists to let finish.
		l.logCounts(ctx, "project event listener stopped")
	}

	for _, subject := range subjects {
		stop, err := subscriber.Subscribe(handlerCtx, subject, queue, l.Handle)
		if err != nil {
			// Undo the ones already made. A half-attached listener is worse
			// than none: it accelerates creation but not updates, which is a
			// difference nobody would think to look for.
			stopAll()
			return nil, err
		}
		stops = append(stops, stop)
	}

	slog.InfoContext(ctx, "project event listener started",
		"subjects", subjects, "queue", queue)

	return stopAll, nil
}

// Handle reconciles the project one event names.
//
// Every failure path here ends in a count and a log rather than a return. There
// is nobody to return to — the transport has no acknowledgement, so a handler
// cannot ask for the message again — and the sweep repairs whatever this drops.
func (l *ProjectListener) Handle(ctx context.Context, data []byte) {
	l.received.Add(1)

	project, err := l.decode(data)
	if err != nil {
		// Includes an event for some other object type, which should not arrive
		// on these subjects at all. Counted separately from a missing stage
		// because it means something different: this one says the subscription
		// is wrong, where a missing stage says the document is thin.
		l.droppedOther.Add(1)
		slog.WarnContext(ctx, "dropped a project event that could not be read",
			"error", err)
		return
	}

	// A stage this service cannot read must not be treated as the project
	// leaving formation. That would freeze a live checklist and lock people out
	// of work in progress, on the strength of a field that is absent from most
	// indexed project documents. Leave the lifecycle where it is and let the
	// sweep, which asks the project service directly rather than reading an
	// indexed copy, decide.
	if project.SubStage == "" {
		l.droppedStage.Add(1)
		slog.DebugContext(ctx, "dropped a project event carrying no stage; the sweep will decide",
			"project_uid", project.UID)
		return
	}

	report := l.reconciler.ReconcileProject(ctx, project, TriggerListener)
	l.handled.Add(1)
	if report.Failed > 0 || report.ProjectionFailed > 0 {
		l.failed.Add(1)
	}

	slog.InfoContext(ctx, "handled a project event",
		"project_uid", project.UID,
		"stage", project.SubStage,
		"created", report.Created,
		"lifecycles_moved", report.LifecyclesMoved,
		"skipped", report.Skipped,
		"blocked", report.Blocked,
		"failed", report.Failed,
		"projected", report.Projected,
		"projection_failed", report.ProjectionFailed,
	)
}

// ListenerCounts is what the listener has done since the process started.
type ListenerCounts struct {
	Received     int64
	Handled      int64
	DroppedStage int64
	DroppedOther int64
	Failed       int64
}

// ReportEvery logs the listener's totals on a ticker until ctx is cancelled.
//
// Started on the sweep's interval, so each daily sweep summary is accompanied by
// what the listener did in the same period and the two can be read against each
// other: a sweep that created checklists next to a listener that received
// nothing is the signature of a listener that has stopped working.
//
// The zero line is the point of this. Per-event logs say what happened; they
// cannot say that nothing happened, and "nothing happened" is exactly the state
// that a dead listener and a quiet day have in common. Emitted unconditionally
// for that reason, rather than only when a count moved.
func (l *ProjectListener) ReportEvery(ctx context.Context, interval time.Duration) {
	// NewTicker panics on a non-positive interval, and this runs in a goroutine
	// where that takes the process with it. RECONCILE_INTERVAL=0s parses
	// cleanly, so the config layer hands it over unchanged.
	if interval <= 0 {
		interval = constants.DefaultReconcileInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The closing summary belongs to the stop function, which knows
			// when the last handler actually finished.
			return
		case <-ticker.C:
			l.logCounts(ctx, "project event listener summary")
		}
	}
}

// logCounts emits the listener's totals under msg.
//
// One function for both the periodic summary and the closing one, because the
// only reason to emit both is that they are comparable. Two copies of the same
// five keys stay comparable exactly as long as nobody edits one of them.
func (l *ProjectListener) logCounts(ctx context.Context, msg string) {
	counts := l.Counts()
	slog.InfoContext(ctx, msg,
		"received", counts.Received, "handled", counts.Handled,
		"dropped_no_stage", counts.DroppedStage,
		"dropped_unreadable", counts.DroppedOther,
		"failed", counts.Failed)
}

// Counts reports the listener's totals, for the periodic summary that makes a
// dead listener visible.
func (l *ProjectListener) Counts() ListenerCounts {
	return ListenerCounts{
		Received:     l.received.Load(),
		Handled:      l.handled.Load(),
		DroppedStage: l.droppedStage.Load(),
		DroppedOther: l.droppedOther.Load(),
		Failed:       l.failed.Load(),
	}
}
