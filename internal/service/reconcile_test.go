// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// listProjects is a ProjectReader whose forming-project list the test controls,
// standing in for the transport that will supply it.
type listProjects struct {
	mu           sync.Mutex
	refs         []port.ProjectRef
	announcement string
	listErr      error
	listCalls    int
}

func (l *listProjects) GetSettings(_ context.Context, projectUID string) (*port.ProjectSettings, error) {
	return &port.ProjectSettings{ProjectUID: projectUID, AnnouncementDate: &l.announcement}, nil
}

func (l *listProjects) ListFormingProjects(_ context.Context) ([]port.ProjectRef, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listCalls++
	if l.listErr != nil {
		return nil, l.listErr
	}
	return l.refs, nil
}

func (l *listProjects) setRefs(refs []port.ProjectRef) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refs = refs
}

func (l *listProjects) calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.listCalls
}

// newReconciler builds a reconciler over the shared expansion fixture.
func newReconciler(t *testing.T, projects port.ProjectReader) (*Reconciler, *expansionFixture) {
	t.Helper()
	f := newExpansionFixture(t, twoItemSections(), projects)
	return NewReconciler(projects, f.expander, NewLifecycler(f.formations)), f
}

// The stage gate: only the four forming stages get a checklist, and the ones
// that do not must not get one as a side effect of being swept.
func TestReconcileCreatesOnlyForFormingStages(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "exploratory", SubStage: model.StageFormationExploratory},
		{UID: "engaged", SubStage: model.StageFormationEngaged},
		{UID: "on-hold", SubStage: model.StageFormationOnHold},
		{UID: "confidential", SubStage: model.StageFormationConfidential},
		{UID: "prospect", SubStage: model.StageProspect},
		{UID: "active", SubStage: model.StageActive},
		{UID: "archived", SubStage: model.StageArchived},
		{UID: "disengaged", SubStage: model.StageFormationDisengaged},
		{UID: "unknown", SubStage: "Something New Upstream"},
	}}
	r, f := newReconciler(t, projects)

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error", err)
	}
	if report.Swept != 9 {
		t.Errorf("swept = %d, want 9", report.Swept)
	}
	if report.Created != 4 {
		t.Errorf("created = %d, want 4", report.Created)
	}

	for _, uid := range []string{"exploratory", "engaged", "on-hold", "confidential"} {
		if _, err := f.formations.GetByProject(ctx, uid); err != nil {
			t.Errorf("%s has no checklist: %v", uid, err)
		}
	}
	// Disengaged carries the formation prefix, so it is the one most likely to
	// be let through by a prefix test rather than the explicit gate.
	for _, uid := range []string{"prospect", "active", "archived", "disengaged", "unknown"} {
		if _, err := f.formations.GetByProject(ctx, uid); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s got a checklist, want none", uid)
		}
	}
}

// The property the reconcile-primary design was chosen for: with no change
// notifications at all — none are wired here — every project still ends up with
// its checklist. If this passes only because a notification fired, the design's
// central claim is untested.
func TestReconcileAloneLandsEveryChecklist(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{}
	r, f := newReconciler(t, projects)

	// Nothing is forming yet.
	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("first sweep = %v, want no error", err)
	}

	// Projects enter formation with nothing telling this service about it.
	projects.setRefs([]port.ProjectRef{
		{UID: "late-1", SubStage: model.StageFormationEngaged},
		{UID: "late-2", SubStage: model.StageFormationConfidential},
	})

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("second sweep = %v, want no error", err)
	}
	if report.Created != 2 {
		t.Errorf("created = %d, want 2", report.Created)
	}
	for _, uid := range []string{"late-1", "late-2"} {
		formation, getErr := f.formations.GetByProject(ctx, uid)
		if getErr != nil {
			t.Fatalf("%s has no checklist: %v", uid, getErr)
		}
		items, listErr := f.items.ListByFormation(ctx, formation.UID)
		if listErr != nil {
			t.Fatalf("listing %s: %v", uid, listErr)
		}
		if len(items) != 3 {
			t.Errorf("%s has %d items, want 3", uid, len(items))
		}
	}
}

// Sweeping repeatedly must not add a second checklist or a second set of items.
func TestReconcileIsIdempotentAcrossSweeps(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
	}}
	r, f := newReconciler(t, projects)

	for i := range 4 {
		report, err := r.ReconcileOnce(ctx)
		if err != nil {
			t.Fatalf("sweep %d = %v, want no error", i, err)
		}
		// Only the first sweep creates anything.
		wantCreated := 0
		if i == 0 {
			wantCreated = 1
		}
		if report.Created != wantCreated {
			t.Errorf("sweep %d created %d, want %d", i, report.Created, wantCreated)
		}
	}

	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	items, err := f.items.ListByFormation(ctx, formation.UID)
	if err != nil {
		t.Fatalf("ListByFormation() = %v, want no error", err)
	}
	if len(items) != 3 {
		t.Errorf("items = %d, want 3", len(items))
	}
}

// Every replica runs this loop with no leader election, so concurrent sweeps are
// the normal case rather than an edge one.
func TestReconcileIsSafeOnEveryReplica(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
		{UID: "project-2", SubStage: model.StageFormationOnHold},
	}}
	r, f := newReconciler(t, projects)

	const replicas = 8
	var wg sync.WaitGroup
	created := make(chan int, replicas)
	for range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report, err := r.ReconcileOnce(ctx)
			if err != nil {
				t.Errorf("ReconcileOnce() = %v, want no error", err)
				return
			}
			created <- report.Created
		}()
	}
	wg.Wait()
	close(created)

	total := 0
	for c := range created {
		total += c
	}
	// Exactly one replica creates each checklist; the rest are absorbed.
	if total != 2 {
		t.Errorf("total created across %d replicas = %d, want 2", replicas, total)
	}

	for _, uid := range []string{"project-1", "project-2"} {
		formation, getErr := f.formations.GetByProject(ctx, uid)
		if getErr != nil {
			t.Fatalf("%s: %v", uid, getErr)
		}
		items, listErr := f.items.ListByFormation(ctx, formation.UID)
		if listErr != nil {
			t.Fatalf("%s: %v", uid, listErr)
		}
		if len(items) != 3 {
			t.Errorf("%s has %d items, want 3 — a concurrent sweep duplicated rows", uid, len(items))
		}
	}
}

// Leaving and re-entering formation. The checklist is never deleted, so the same
// one has to come back rather than a fresh one being built.
func TestReconcileMovesLifecycleAndRestoresOnReEntry(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
	}}
	r, f := newReconciler(t, projects)

	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("create sweep = %v, want no error", err)
	}
	original, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	if original.Lifecycle != model.LifecycleLive {
		t.Fatalf("lifecycle = %q, want live", original.Lifecycle)
	}

	transitions := []struct {
		stage string
		want  model.Lifecycle
	}{
		{model.StageActive, model.LifecycleCompleted},
		{model.StageFormationEngaged, model.LifecycleLive},
		{model.StageArchived, model.LifecycleFrozen},
		{model.StageFormationConfidential, model.LifecycleLive},
		{model.StageFormationDisengaged, model.LifecycleFrozen},
		{model.StageFormationOnHold, model.LifecycleLive},
	}

	for _, tr := range transitions {
		t.Run(tr.stage, func(t *testing.T) {
			projects.setRefs([]port.ProjectRef{{UID: "project-1", SubStage: tr.stage}})
			report, sweepErr := r.ReconcileOnce(ctx)
			if sweepErr != nil {
				t.Fatalf("sweep = %v, want no error", sweepErr)
			}
			if report.LifecyclesMoved != 1 {
				t.Errorf("lifecycles moved = %d, want 1", report.LifecyclesMoved)
			}

			after, getErr := f.formations.GetByProject(ctx, "project-1")
			if getErr != nil {
				t.Fatalf("GetByProject() = %v, want no error", getErr)
			}
			if after.Lifecycle != tr.want {
				t.Errorf("lifecycle = %q, want %q", after.Lifecycle, tr.want)
			}
			// The same checklist throughout: never deleted, never rebuilt.
			if after.UID != original.UID {
				t.Errorf("formation uid = %v, want the original %v", after.UID, original.UID)
			}

			items, listErr := f.items.ListByFormation(ctx, after.UID)
			if listErr != nil {
				t.Fatalf("ListByFormation() = %v, want no error", listErr)
			}
			if len(items) != 3 {
				t.Errorf("items = %d, want the original 3", len(items))
			}
		})
	}
}

// An unrecognised stage must not freeze a live checklist. Reading a value this
// service has not been taught is not evidence the project left formation, and
// freezing on it would lock people out of work in progress.
func TestReconcileLeavesLifecycleAloneOnAnUnknownStage(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
	}}
	r, f := newReconciler(t, projects)
	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("create sweep = %v, want no error", err)
	}

	projects.setRefs([]port.ProjectRef{{UID: "project-1", SubStage: "Formation-Engaged"}})
	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("sweep = %v, want no error", err)
	}
	if report.LifecyclesMoved != 0 {
		t.Errorf("lifecycles moved = %d, want 0", report.LifecyclesMoved)
	}

	after, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	if after.Lifecycle != model.LifecycleLive {
		t.Errorf("lifecycle = %q, want live — an unreadable stage froze a live checklist", after.Lifecycle)
	}
}

// One project failing must not abandon the others in the same sweep.
func TestReconcileContinuesPastOneFailure(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
		{UID: "project-2", SubStage: model.StageFormationEngaged},
	}}
	// Built without the fixture, which seeds a template: with no published
	// template, expansion fails for every project, which is the condition
	// under test.
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	templates := mock.NewTemplateRepository()
	uow := mock.NewUnitOfWork(formations, items, mock.NewActivityRepository(), templates)

	r := NewReconciler(
		projects,
		NewExpander(NewTemplateSelector(templates), uow, projects),
		NewLifecycler(formations),
	)

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error — a per-project failure must not end the sweep", err)
	}
	if report.Swept != 2 {
		t.Errorf("swept = %d, want 2", report.Swept)
	}
	if report.Failed != 2 {
		t.Errorf("failed = %d, want 2", report.Failed)
	}
}

// A failure listing projects ends that sweep but must be reported, not hidden.
func TestReconcileReportsAListingFailure(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{listErr: errors.New("upstream unavailable")}
	r, _ := newReconciler(t, projects)

	if _, err := r.ReconcileOnce(ctx); err == nil {
		t.Error("ReconcileOnce() = nil, want the listing error")
	}
}

// With nothing able to supply the project list, a sweep must report having swept
// nothing rather than look like a successful empty sweep.
func TestReconcileWithNoProjectReader(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), nil)
	r := NewReconciler(nil, f.expander, NewLifecycler(f.formations))

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error", err)
	}
	if report.Swept != 0 || report.Created != 0 {
		t.Errorf("report = %+v, want an empty sweep", report)
	}
}

// Run sweeps immediately rather than waiting out the first interval, and stops
// when its context is cancelled.
func TestRunSweepsImmediatelyAndStopsOnCancel(t *testing.T) {
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
	}}
	r, f := newReconciler(t, projects)
	// Long enough that a second tick cannot be what creates the checklist.
	r.interval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()

	deadline := time.After(2 * time.Second)
	for {
		if _, err := f.formations.GetByProject(context.Background(), "project-1"); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no checklist after 2s; Run did not sweep before its first tick")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Run did not return after its context was cancelled")
	}

	if projects.calls() == 0 {
		t.Error("Run never listed projects")
	}
}
