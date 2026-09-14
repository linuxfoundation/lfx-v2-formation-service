// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// refresherFixture is a refresher over the same mocks the projection tests use,
// with one project holding a two-item checklist.
type refresherFixture struct {
	refresher  *Refresher
	projects   *mock.ProjectReader
	publisher  *mock.IndexerPublisher
	formations *mock.FormationRepository
	items      *mock.ItemRepository
	formation  *model.Formation
}

// newRefresherFixture seeds a project with a checklist and wires a refresher
// over a real projector.
//
// A real projector rather than a double, deliberately: the claim this feature
// makes is that a write publishes the same documents a sweep publishes, and a
// fake projector would let that claim hold in the tests and fail in the
// deployment.
func newRefresherFixture(t *testing.T) *refresherFixture {
	t.Helper()
	ctx := context.Background()

	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	publisher := mock.NewIndexerPublisher()
	projects := mock.NewProjectReader()
	projects.SetName("project-1", "A Project")
	projects.SetProjectsByUID([]port.ProjectRef{{
		UID: "project-1", Slug: "a-project", SubStage: model.StageFormationEngaged,
	}})

	formation, err := formations.Create(ctx, &model.Formation{ProjectUID: "project-1"})
	if err != nil {
		t.Fatalf("seeding the formation = %v", err)
	}
	if _, err := items.InsertMany(ctx, []*model.Item{
		{FormationUID: formation.UID, ItemKey: "a", Title: "Item A", Status: model.StatusNotStarted},
		{FormationUID: formation.UID, ItemKey: "b", Title: "Item B", Status: model.StatusNotStarted},
	}); err != nil {
		t.Fatalf("seeding the items = %v", err)
	}

	return &refresherFixture{
		refresher:  NewRefresher(projects, NewProjector(formations, items, projects, publisher)),
		projects:   projects,
		publisher:  publisher,
		formations: formations,
		items:      items,
		formation:  formation,
	}
}

// drain runs Stop, which is how these tests wait for an asynchronous refresh
// without sleeping for a duration that is either flaky or slow.
func (f *refresherFixture) drain(t *testing.T) {
	t.Helper()
	f.refresher.Stop(context.Background())
}

// blockingProjects holds GetRef until released, so a test can observe the
// moment between "the write returned" and "the refresh finished" — which is
// the whole behaviour under test and is otherwise unobservable.
type blockingProjects struct {
	*mock.ProjectReader
	release chan struct{}
}

func (b blockingProjects) GetRef(ctx context.Context, projectUID string) (port.ProjectRef, error) {
	<-b.release
	return b.ProjectReader.GetRef(ctx, projectUID)
}

// The refresh publishes the whole project, not only the item that was written:
// the checklist document carries counts over every item, so a write that
// republished one item would leave the aggregate disagreeing with the rows it
// is an aggregate of.
func TestAfterItemWritePublishesTheWholeProject(t *testing.T) {
	f := newRefresherFixture(t)

	f.refresher.AfterItemWrite(context.Background(), "project-1")
	f.drain(t)

	if got := f.publisher.Count(); got != 1 {
		t.Errorf("checklist publishes = %d, want 1", got)
	}
	if got := f.publisher.ItemCount(); got != 2 {
		t.Errorf("item publishes = %d, want 2 (one per item on the checklist)", got)
	}
	doc := f.publisher.Latest("project-1")
	if doc == nil {
		t.Fatal("no checklist document published for project-1")
	}
	// The ref came from the lookup this feature added, so these two fields are
	// the evidence it was actually consulted rather than defaulted.
	if doc.ProjectSlug != "a-project" {
		t.Errorf("project_slug = %q, want a-project", doc.ProjectSlug)
	}

	want := RefreshCounts{Requested: 1, Published: 1}
	if got := f.refresher.Counts(); got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}

// The call returns before the refresh completes. It is made on the request
// goroutine after a write has already been committed, so blocking it would put
// three NATS round trips in front of a response whose answer is already known.
func TestAfterItemWriteDoesNotWaitForTheRefresh(t *testing.T) {
	f := newRefresherFixture(t)
	gate := blockingProjects{ProjectReader: f.projects, release: make(chan struct{})}
	f.refresher.projects = gate

	f.refresher.AfterItemWrite(context.Background(), "project-1")

	// Nothing can have been published: the lookup has not returned. Asserted
	// without a sleep — the gate is what makes this deterministic rather than a
	// race the test usually wins.
	if got := f.publisher.Count(); got != 0 {
		t.Errorf("published %d documents before the lookup returned; the call did not return early", got)
	}

	close(gate.release)
	f.drain(t)

	if got := f.publisher.Count(); got != 1 {
		t.Errorf("checklist publishes = %d, want 1 once the refresh completed", got)
	}
}

// Goa cancels the request context once the response is written. A refresh
// holding that context directly would find it cancelled before doing anything,
// so the refresher would look wired and silently never publish.
func TestAfterItemWriteSurvivesTheRequestContextBeingCancelled(t *testing.T) {
	f := newRefresherFixture(t)
	gate := blockingProjects{ProjectReader: f.projects, release: make(chan struct{})}
	f.refresher.projects = gate

	ctx, cancel := context.WithCancel(context.Background())
	f.refresher.AfterItemWrite(ctx, "project-1")

	// Cancelled while the refresh is held at the gate, which is the ordering
	// that matters: the response has been written and the refresh has not yet
	// done its work.
	cancel()
	close(gate.release)
	f.drain(t)

	if got := f.publisher.Count(); got != 1 {
		t.Errorf("checklist publishes = %d, want 1; the refresh must outlive the request", got)
	}
}

// A failed publish is counted and logged, never returned. The item is committed
// and the response has been decided; all that is lost is freshness, which the
// next sweep restores.
func TestAfterItemWriteCountsAFailedPublish(t *testing.T) {
	f := newRefresherFixture(t)
	f.publisher.SetError(errors.New("the index is unreachable"))

	f.refresher.AfterItemWrite(context.Background(), "project-1")
	f.drain(t)

	want := RefreshCounts{Requested: 1, Failed: 1}
	if got := f.refresher.Counts(); got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}

// A project that cannot be read is a failure; a project that is not there is
// not. Kept apart because the first leaves a stale document behind and the
// second leaves nothing at all — and a checklist outlives its project, so the
// second is a state a deployed service reaches.
func TestAfterItemWriteSeparatesAnAbsentProjectFromAnUnreachableOne(t *testing.T) {
	tests := []struct {
		name  string
		arm   func(f *refresherFixture)
		want  RefreshCounts
		named string
	}{
		{
			name:  "the project does not exist",
			arm:   func(*refresherFixture) {},
			named: "never-seeded",
			want:  RefreshCounts{Requested: 1, Skipped: 1},
		},
		{
			name:  "the project service is unreachable",
			arm:   func(f *refresherFixture) { f.projects.SetRefError(errors.New("no responders")) },
			named: "project-1",
			want:  RefreshCounts{Requested: 1, Failed: 1},
		},
		{
			name: "the project holds no checklist",
			arm: func(f *refresherFixture) {
				f.projects.SetProjectsByUID([]port.ProjectRef{{UID: "empty-project", Slug: "empty"}})
			},
			named: "empty-project",
			want:  RefreshCounts{Requested: 1, Skipped: 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRefresherFixture(t)
			tc.arm(f)

			f.refresher.AfterItemWrite(context.Background(), tc.named)
			f.drain(t)

			if got := f.refresher.Counts(); got != tc.want {
				t.Errorf("counts = %+v, want %+v", got, tc.want)
			}
			if got := f.publisher.Count(); got != 0 {
				t.Errorf("published %d documents, want none", got)
			}
		})
	}
}

// After Stop, a refresh is refused rather than started. A goroutine spawned
// after the drain has finished is one nothing is waiting for, and it would be
// reading a database pool that is about to be released.
func TestStopRefusesLaterRefreshes(t *testing.T) {
	f := newRefresherFixture(t)
	f.drain(t)

	f.refresher.AfterItemWrite(context.Background(), "project-1")

	want := RefreshCounts{Requested: 1, Dropped: 1}
	if got := f.refresher.Counts(); got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
	if got := f.publisher.Count(); got != 0 {
		t.Errorf("published %d documents after shutdown, want none", got)
	}
}

// Stop waits for a refresh already running rather than abandoning it, which is
// the only reason the drain exists: a write that has just been acknowledged
// should reach the index even if the acknowledgement was the pod's last act.
func TestStopWaitsForARefreshAlreadyRunning(t *testing.T) {
	f := newRefresherFixture(t)
	gate := blockingProjects{ProjectReader: f.projects, release: make(chan struct{})}
	f.refresher.projects = gate

	f.refresher.AfterItemWrite(context.Background(), "project-1")

	// Released from another goroutine so Stop is genuinely blocked when it is
	// released, rather than finding the work already done.
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(gate.release)
	}()
	f.drain(t)

	if got := f.publisher.Count(); got != 1 {
		t.Errorf("checklist publishes = %d, want 1; Stop returned before the refresh finished", got)
	}
}

// stuckProjects holds GetRef until its context is cancelled, standing in for a
// refresh blocked on an unreachable project service — the case the drain budget
// exists for, and the only one in which it expires.
type stuckProjects struct {
	*mock.ProjectReader
	entered chan struct{}
	once    sync.Once
}

func (s *stuckProjects) GetRef(ctx context.Context, _ string) (port.ProjectRef, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return port.ProjectRef{}, ctx.Err()
}

// When the drain budget expires the refresh is cancelled, and Stop does not
// return until it has actually exited.
//
// Returning while it ran would make the budget bound nothing. The refresh holds
// a database connection, the caller closes the pool as soon as Stop returns,
// and that close waits for the connection to come back — so the wait would
// reappear during teardown where the pod's grace period is all that is left to
// absorb it, which is how a drain documented as five seconds becomes fifteen.
func TestStopCancelsARefreshThatOutlastsTheDrainBudget(t *testing.T) {
	f := newRefresherFixture(t)
	stuck := &stuckProjects{ProjectReader: f.projects, entered: make(chan struct{})}
	f.refresher.projects = stuck
	f.refresher.drainTimeout = 10 * time.Millisecond

	f.refresher.AfterItemWrite(context.Background(), "project-1")
	<-stuck.entered

	// The refresh is blocked on a context only Stop can cancel, so this
	// returning at all is the assertion: were the cancel missing, it would sit
	// here until the refresh's own 15-second deadline.
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.refresher.Stop(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return; the refresh was left running rather than cancelled")
	}

	// Counted as a failure rather than passed over: the documents are stale
	// until the next sweep, which is the thing the counters exist to show.
	if got := f.refresher.Counts().Failed; got != 1 {
		t.Errorf("failed refreshes = %d, want 1", got)
	}
}

// Nothing wired is a supported deployment, not a broken one: a service with no
// NATS still accepts writes and still reaches the index at the next sweep.
func TestARefresherWithNothingWiredIsInert(t *testing.T) {
	ctx := context.Background()

	var absent *Refresher
	absent.AfterItemWrite(ctx, "project-1")
	absent.Stop(ctx)
	if got := absent.Counts(); got != (RefreshCounts{}) {
		t.Errorf("counts = %+v, want zero", got)
	}

	unwired := NewRefresher(nil, nil)
	unwired.AfterItemWrite(ctx, "project-1")
	unwired.Stop(ctx)
	if got := unwired.Counts(); got != (RefreshCounts{}) {
		t.Errorf("counts = %+v, want zero", got)
	}
}

// An empty project UID asks for a refresh of nothing. Not counted as requested,
// because there is nothing a reader of that number could do about it.
func TestAfterItemWriteIgnoresAnEmptyProjectUID(t *testing.T) {
	f := newRefresherFixture(t)

	f.refresher.AfterItemWrite(context.Background(), "")
	f.drain(t)

	if got := f.refresher.Counts(); got != (RefreshCounts{}) {
		t.Errorf("counts = %+v, want zero", got)
	}
}

// panickingProjects raises a panic where a NATS adapter would have made a
// request, standing in for any latent bug on the refresh path.
type panickingProjects struct {
	*mock.ProjectReader
}

func (p panickingProjects) GetRef(context.Context, string) (port.ProjectRef, error) {
	panic("a latent bug on the refresh path")
}

// A panic on the refresh goroutine must not take the process down.
//
// net/http recovers a panic raised while serving a request, so before this work
// moved off the request goroutine the same bug cost one request. Off it, an
// unrecovered panic kills the pod — and it would be reachable by anyone able to
// write an item, repeatedly. The test passing at all is the assertion: an
// unrecovered panic in a goroutine fails the whole test binary.
func TestAPanicDuringRefreshDoesNotKillTheProcess(t *testing.T) {
	f := newRefresherFixture(t)
	f.refresher.projects = panickingProjects{ProjectReader: f.projects}

	f.refresher.AfterItemWrite(context.Background(), "project-1")
	f.drain(t)

	// Counted as a failure rather than swallowed, so a refresher panicking on
	// every write is visible in the summary instead of only in the logs.
	want := RefreshCounts{Requested: 1, Failed: 1}
	if got := f.refresher.Counts(); got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}

// The periodic summary reports the interval, not the totals. Reporting totals
// would make a quiet interval indistinguishable from a busy one in a service
// that has been up for a week.
func TestRefreshCountsSinceReportsTheInterval(t *testing.T) {
	earlier := RefreshCounts{Requested: 10, Published: 7, Skipped: 1, Failed: 2, Dropped: 0}
	current := RefreshCounts{Requested: 14, Published: 10, Skipped: 1, Failed: 3, Dropped: 1}

	want := RefreshCounts{Requested: 4, Published: 3, Skipped: 0, Failed: 1, Dropped: 1}
	if got := current.since(earlier); got != want {
		t.Errorf("since() = %+v, want %+v", got, want)
	}
}

// RECONCILE_INTERVAL=0s parses cleanly and reaches here unchanged, and
// NewTicker panics on it — in a goroutine, which takes the process down.
func TestReportEveryToleratesANonPositiveInterval(t *testing.T) {
	f := newRefresherFixture(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.refresher.ReportEvery(ctx, 0)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReportEvery did not return when its context was cancelled")
	}
}
