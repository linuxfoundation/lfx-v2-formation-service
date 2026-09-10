// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"sort"
	"testing"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// What one event costs when the project it names already has its checklist.
//
// The case that decides whether the accelerator is affordable, because it is
// almost every event. A project is edited many times over its life and needs a
// checklist created once, so the steady state of the listener is being told
// about a project that needs nothing — and the sweep's reads were priced on
// being made once for every project, not once per project per event.
//
// The service has to absorb a full-catalogue republish without degrading the
// sweep. A republish delivers one event per project, so this number multiplied
// by the catalogue is the load that actually arrives: at the 1032 project
// documents in the dev index, a per-event transaction is a thousand
// transactions arriving as fast as NATS can deliver them.
//
// Written as an exact ledger rather than a ceiling. A budget of "no more than
// eight" would be satisfied by the reads moving somewhere else, and the point is
// to notice when the cost changes at all — including a read reappearing that was
// deliberately removed, which is invisible in a diff and shows up in production
// only as database load nobody connects to this path.
func TestOneEventForASettledProjectCostsAKnownAmountOfIO(t *testing.T) {
	ctx := context.Background()
	project := port.ProjectRef{UID: "settled", Slug: "settled", SubStage: model.StageFormationEngaged}
	projects := &listProjects{refs: []port.ProjectRef{project}}
	r, f, publisher := newReconcilerWithIndex(t, projects)
	uow, ok := f.uow.(*mock.UnitOfWork)
	if !ok {
		t.Fatalf("fixture unit of work is %T, want the mock so its transactions can be counted", f.uow)
	}

	// Arrange by reconciling once, which is what leaves the project in the state
	// this test is about. Its cost is not the measurement, so the ledgers are
	// emptied afterwards rather than before.
	if report := r.ReconcileProject(ctx, project, TriggerListener); report.Created != 1 {
		t.Fatalf("arranging the checklist: created = %d, want 1", report.Created)
	}
	recorders := []mock.CallRecorder{f.formations, f.items, f.templates, f.activity, uow}
	mock.ResetAll(recorders...)
	publishesBefore := publisher.Count()
	nameCallsBefore := projects.nameCalls()

	report := r.ReconcileProject(ctx, project, TriggerListener)

	if report.Created != 0 {
		t.Errorf("created = %d, want 0 — the checklist already exists", report.Created)
	}
	ledger := mock.Ledger(recorders...)
	logLedger(t, ledger)

	want := map[string]int{
		"formations.GetByProject": 3,
		"items.ListByFormation":   1,
	}
	for name, wantCount := range want {
		if ledger[name] != wantCount {
			t.Errorf("%s called %d times, want %d", name, ledger[name], wantCount)
		}
	}
	for name, count := range ledger {
		if _, expected := want[name]; !expected {
			t.Errorf("%s called %d times, want none — an unbudgeted read is on the event path", name, count)
		}
	}
	// Four, from eight. The three that went are the whole-table read of every
	// project UID, the full template list, and the platform pass's transaction
	// with its two queries inside it.
	//
	// The three remaining reads of the same formation row are known and left:
	// the existence check, the lifecycle sync and the projection each read it
	// independently. Collapsing them means passing the row through three port
	// signatures, which is a wider change than this.
	if got := mock.Total(ledger); got != 4 {
		t.Errorf("database calls = %d, want 4", got)
	}

	// The transaction was the expensive half, and it is the platform pass's. It
	// does not run here because a project event tells it nothing: whether a
	// committee or a mailing list exists does not change because a project
	// document did. The sweep still asks, daily.
	if got := ledger["uow.Do"]; got != 0 {
		t.Errorf("transactions = %d, want 0", got)
	}

	// The NATS side, which is smaller but not free: the queue row is
	// republished unconditionally so a lost projection repairs itself, and the
	// display name is a request the list reply does not carry.
	if got := publisher.Count() - publishesBefore; got != 1 {
		t.Errorf("index publishes = %d, want 1", got)
	}
	if got := projects.nameCalls() - nameCallsBefore; got != 1 {
		t.Errorf("project name lookups = %d, want 1", got)
	}
}

// What one sweep of one settled project costs, which must not have grown.
//
// The sweep pays the reads the event path is being relieved of — the checklist
// set and the template — and it is right to: it amortizes them over every
// project, which is the reason they were hoisted out of the loop. This test is
// here so that making the event path cheaper cannot be done by moving its cost
// into the sweep, where a regression would be a day's worth of load rather than
// an event's.
func TestOneSweepOfASettledProjectCostsAKnownAmountOfIO(t *testing.T) {
	ctx := context.Background()
	project := port.ProjectRef{UID: "settled", Slug: "settled", SubStage: model.StageFormationEngaged}
	projects := &listProjects{refs: []port.ProjectRef{project}}
	r, f, _ := newReconcilerWithIndex(t, projects)
	uow, ok := f.uow.(*mock.UnitOfWork)
	if !ok {
		t.Fatalf("fixture unit of work is %T, want the mock so its transactions can be counted", f.uow)
	}

	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("arranging the checklist: %v", err)
	}
	recorders := []mock.CallRecorder{f.formations, f.items, f.templates, f.activity, uow}
	mock.ResetAll(recorders...)

	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error", err)
	}

	ledger := mock.Ledger(recorders...)
	logLedger(t, ledger)

	// Seven, from eight: only the template read went, and only because nothing
	// needed creating. The sweep keeps the platform pass — its transaction and
	// the two queries in it — which is the difference between this ledger and
	// the event path's, and it is deliberate. The pass is what measures how many
	// rows are waiting on a lookup nobody has written yet, and once a day per
	// project is what that measurement costs.
	want := map[string]int{
		"formations.ListProjectUIDs": 1,
		"formations.GetByProject":    3,
		"items.ListByFormation":      2,
		"uow.Do":                     1,
	}
	for name, wantCount := range want {
		if ledger[name] != wantCount {
			t.Errorf("%s called %d times, want %d", name, ledger[name], wantCount)
		}
	}
	for name, count := range ledger {
		if _, expected := want[name]; !expected {
			t.Errorf("%s called %d times, want none — an unbudgeted read is in the sweep", name, count)
		}
	}
	if got := mock.Total(ledger); got != 7 {
		t.Errorf("database calls = %d, want 7", got)
	}
	if got := ledger["templates.ListPublished"]; got != 0 {
		t.Errorf("template reads = %d, want 0 — nothing in this sweep needed creating", got)
	}
}

// The platform pass runs on a schedule, not on a project event.
//
// The one step the accelerator does not accelerate, and the reason is what makes
// it safe rather than a saving taken carelessly. The pass asks other services
// whether a repository, a mailing list or a committee exists yet; none of those
// is created or removed because a project document changed, so an event carries
// no news about any of them. Running the pass on receipt re-asks a question
// whose answer did not move.
//
// The consequence is bounded and is the same bound the rest of this design
// accepts: a committee created today is reflected on the checklist by the next
// sweep. Nothing is lost, only deferred, and the sweep is the mechanism of
// record for exactly this reason.
//
// Asserted through the report rather than through call counts, so it stays a
// statement about behaviour and not only about cost.
func TestAProjectEventDoesNotRunThePlatformPass(t *testing.T) {
	ctx := context.Background()
	project := port.ProjectRef{UID: "project-1", Slug: "p1", SubStage: model.StageFormationEngaged}
	projects := &listProjects{refs: []port.ProjectRef{project}}
	r, _ := newReconciler(t, projects)

	// The fixture's template carries one row nobody can answer for, so a pass
	// that ran would report it — which is what the sweep's own test asserts.
	fromEvent := r.ReconcileProject(ctx, project, TriggerListener)
	if fromEvent.Created != 1 {
		t.Fatalf("created = %d, want 1", fromEvent.Created)
	}
	if fromEvent.PlatformUnanswerable != 0 {
		t.Errorf("platform_unanswerable = %d, want 0 — the pass ran on an event that cannot inform it",
			fromEvent.PlatformUnanswerable)
	}

	// An operator asking for one project by hand is asking for everything the
	// sweep would do, so the pass runs for them.
	fromOperator := r.ReconcileProject(ctx, project, TriggerOperator)
	if fromOperator.PlatformUnanswerable != 1 {
		t.Errorf("platform_unanswerable = %d, want 1 — an operator's reconcile skipped the pass",
			fromOperator.PlatformUnanswerable)
	}
}

// logLedger prints the ledger so a run shows what was counted, not only which
// assertion failed. What makes the number reproducible by hand rather than
// something to take on trust.
func logLedger(t *testing.T, ledger map[string]int) {
	t.Helper()

	names := make([]string, 0, len(ledger))
	for name := range ledger {
		names = append(names, name)
	}
	sort.Strings(names)

	t.Logf("database calls: %d", mock.Total(ledger))
	for _, name := range names {
		t.Logf("  %s: %d", name, ledger[name])
	}
}
