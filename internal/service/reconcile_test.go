// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// listProjects is a ProjectReader whose forming-project list the test controls,
// standing in for the transport that supplies it.
type listProjects struct {
	mu           sync.Mutex
	refs         []port.ProjectRef
	announcement string
	listErr      error
	listCalls    int
	lastAlsoUIDs []string
}

func (l *listProjects) GetSettings(_ context.Context, projectUID string) (*port.ProjectSettings, error) {
	return &port.ProjectSettings{ProjectUID: projectUID, AnnouncementDate: &l.announcement}, nil
}

// ListFormingProjects returns whatever a test set, including projects at stages
// no longer in formation, and records the UIDs it was asked to include.
//
// Returning Active and Archived projects is no longer an overreach: the sweep
// names the projects it holds a checklist for, and the subject answers with
// their current stage whatever it is. That is what makes the completed and
// frozen branches reachable in a deployed pod, where a forming-only list left
// them correct but dead. The recorded UIDs are how a test checks the sweep
// actually asks — the branches would go quietly dead again if it stopped.
func (l *listProjects) ListFormingProjects(_ context.Context, alsoUIDs []string) ([]port.ProjectRef, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listCalls++
	l.lastAlsoUIDs = append([]string{}, alsoUIDs...)
	if l.listErr != nil {
		return nil, l.listErr
	}
	return l.refs, nil
}

// askedFor reports the UIDs the most recent sweep asked to have included.
func (l *listProjects) askedFor() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.lastAlsoUIDs...)
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
	return NewReconciler(projects, f.formations, f.expander, NewLifecycler(f.formations), time.Minute), f
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

// The regression the two tests above could not catch on their own: they hand the
// sweep an Active project directly, so they prove the lifecycle mapping without
// proving the sweep can ever see such a project.
//
// Here the reader applies the real contract. Once project-1 leaves formation it
// is no longer in the stage-filtered list, and the only way it comes back is the
// sweep naming it as a project it holds a checklist for. Drop that and this test
// sees a checklist stuck at live — which is what a deployed pod did.
func TestReconcileCompletesAChecklistAfterItsProjectLeavesFormation(t *testing.T) {
	ctx := context.Background()
	projects := mock.NewProjectReader()
	projects.SetFormingProjects([]port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
	})
	r, f := newReconciler(t, projects)

	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("create sweep = %v, want no error", err)
	}
	created, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	if created.Lifecycle != model.LifecycleLive {
		t.Fatalf("lifecycle = %q, want live", created.Lifecycle)
	}

	// The project goes Active. It leaves the formation stages entirely, which is
	// exactly the state a forming-only list cannot represent.
	projects.SetFormingProjects(nil)
	projects.SetProjectsByUID([]port.ProjectRef{{UID: "project-1", SubStage: model.StageActive}})

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("sweep after leaving formation = %v, want no error", err)
	}
	if report.Swept != 1 {
		t.Fatalf("swept = %d, want 1 — the project is only in the list because the sweep named it",
			report.Swept)
	}
	if report.LifecyclesMoved != 1 {
		t.Errorf("lifecycles moved = %d, want 1", report.LifecyclesMoved)
	}

	after, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	if after.Lifecycle != model.LifecycleCompleted {
		t.Errorf("lifecycle = %q, want completed", after.Lifecycle)
	}
	// The same checklist, moved rather than rebuilt.
	if after.UID != created.UID {
		t.Errorf("formation uid = %v, want the original %v", after.UID, created.UID)
	}
}

// The sweep must name the projects it holds a checklist for, or the reach above
// goes quietly dead: every assertion still passes against a fake that ignores
// the argument, and nothing else would notice.
func TestReconcileNamesTheProjectsItHoldsChecklistsFor(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
	}}
	r, _ := newReconciler(t, projects)

	// The first sweep holds nothing yet, so it asks for the stages alone.
	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("create sweep = %v, want no error", err)
	}
	if asked := projects.askedFor(); len(asked) != 0 {
		t.Errorf("first sweep asked for %v, want nothing — no checklist existed to name", asked)
	}

	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("second sweep = %v, want no error", err)
	}
	asked := projects.askedFor()
	if len(asked) != 1 || asked[0] != "project-1" {
		t.Errorf("second sweep asked for %v, want [project-1] — the checklist it now holds", asked)
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

// The interval is configuration, and the startup log reports it — so a value
// that arrives from the environment has to reach the ticker rather than being
// parsed into a field nothing reads, which is what happened before.
func TestReconcilerUsesTheConfiguredInterval(t *testing.T) {
	projects := &listProjects{}
	f := newExpansionFixture(t, twoItemSections(), projects)

	r := NewReconciler(projects, f.formations, f.expander, NewLifecycler(f.formations), 90*time.Second)
	if r.interval != 90*time.Second {
		t.Errorf("interval = %v, want 90s", r.interval)
	}

	// A zero or negative duration would panic time.NewTicker, so an unparseable
	// or absent setting falls back rather than taking the loop down.
	for _, bad := range []time.Duration{0, -time.Minute} {
		r := NewReconciler(projects, f.formations, f.expander, NewLifecycler(f.formations), bad)
		if r.interval != constants.DefaultReconcileInterval {
			t.Errorf("interval for %v = %v, want the default %v", bad, r.interval, constants.DefaultReconcileInterval)
		}
	}
}

// failingListFormations answers ListProjectUIDs with an error and otherwise
// behaves normally, so a sweep can be driven with only that read broken.
type failingListFormations struct {
	port.FormationRepository
	err error
}

func (f *failingListFormations) ListProjectUIDs(_ context.Context) ([]string, error) {
	return nil, f.err
}

// Listing the existing checklists is an optimisation, so failing to read it must
// cost efficiency and nothing else: every forming project is attempted, and the
// uniqueness constraint absorbs the ones that already exist. That is what the
// sweep did before the diff, so the fallback is the old behaviour rather than a
// stalled loop.
func TestReconcileStillCreatesWhenTheExistingListCannotBeRead(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
		{UID: "project-2", SubStage: model.StageFormationEngaged},
	}}
	f := newExpansionFixture(t, twoItemSections(), projects)
	broken := &failingListFormations{FormationRepository: f.formations, err: errors.New("connection reset")}

	r := NewReconciler(projects, broken, f.expander, NewLifecycler(f.formations), time.Minute)

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error — a broken optimisation must not fail the sweep", err)
	}
	if report.Created != 2 {
		t.Errorf("created = %d, want 2", report.Created)
	}

	// And a second sweep is still safe: the constraint, not the diff, is what
	// makes repeated attempts a no-op.
	again, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("second sweep = %v, want no error", err)
	}
	if again.Created != 0 {
		t.Errorf("created = %d on the second sweep, want 0", again.Created)
	}
	if again.Failed != 0 {
		t.Errorf("failed = %d, want 0 — an existing checklist is success, not failure", again.Failed)
	}
}

// A template read that fails for a reason other than "none published" must also
// leave the loop running and lifecycles moving.
func TestReconcileSyncsLifecyclesWhenTemplatesCannotBeRead(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{}
	f := newExpansionFixture(t, twoItemSections(), projects)

	if _, err := f.formations.Create(ctx, &model.Formation{
		ProjectUID: "project-2",
		Lifecycle:  model.LifecycleLive,
		Revision:   1,
	}); err != nil {
		t.Fatalf("seeding a formation = %v", err)
	}
	projects.setRefs([]port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
		{UID: "project-2", SubStage: model.StageActive},
	})

	broken := &failingTemplates{err: errors.New("statement timeout")}
	r := NewReconciler(
		projects,
		f.formations,
		NewExpander(NewTemplateSelector(broken), f.uow, projects),
		NewLifecycler(f.formations),
		time.Minute,
	)

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error", err)
	}
	if report.Created != 0 {
		t.Errorf("created = %d, want 0", report.Created)
	}
	if report.LifecyclesMoved != 1 {
		t.Errorf("lifecycles_moved = %d, want 1 — a template failure must not stop lifecycle sync", report.LifecyclesMoved)
	}
}

// failingTemplates fails every read.
type failingTemplates struct {
	port.TemplateRepository
	err error
}

func (f *failingTemplates) ListPublished(_ context.Context) ([]*model.Template, error) {
	return nil, f.err
}

// countingTemplates counts the reads a sweep makes, which is the only way to
// observe that selection is resolved once per sweep rather than once per
// project — the outcome is identical either way.
type countingTemplates struct {
	port.TemplateRepository
	mu    sync.Mutex
	lists int
}

func (c *countingTemplates) ListPublished(ctx context.Context) ([]*model.Template, error) {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return c.TemplateRepository.ListPublished(ctx)
}

func (c *countingTemplates) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists
}

// The template is the same for every project in a sweep — the selector takes no
// project facts at all — so it is read once however many projects there are.
// Reading it per project meant a query and a full decode of the sections
// document for each one, on every tick, forever.
func TestReconcileReadsTheTemplateOncePerSweep(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
		{UID: "project-2", SubStage: model.StageFormationOnHold},
		{UID: "project-3", SubStage: model.StageFormationConfidential},
		{UID: "project-4", SubStage: model.StageFormationExploratory},
	}}
	f := newExpansionFixture(t, twoItemSections(), projects)
	counting := &countingTemplates{TemplateRepository: f.templates}

	r := NewReconciler(
		projects,
		f.formations,
		NewExpander(NewTemplateSelector(counting), f.uow, projects),
		NewLifecycler(f.formations),
		time.Minute,
	)

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error", err)
	}
	if report.Created != 4 {
		t.Fatalf("created = %d, want 4", report.Created)
	}
	if got := counting.count(); got != 1 {
		t.Errorf("template read %d times for a 4-project sweep, want 1", got)
	}
}

// A second sweep over projects that already have checklists must not attempt
// creation again. The uniqueness constraint would absorb it, but paying for a
// transaction and a discarded violation per project per tick is the cost this
// avoids once the backlog is drained — which is the steady state.
func TestReconcileSkipsProjectsThatAlreadyHaveChecklists(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
		{UID: "project-2", SubStage: model.StageFormationOnHold},
	}}
	f := newExpansionFixture(t, twoItemSections(), projects)
	counting := &countingTemplates{TemplateRepository: f.templates}
	r := NewReconciler(
		projects,
		f.formations,
		NewExpander(NewTemplateSelector(counting), f.uow, projects),
		NewLifecycler(f.formations),
		time.Minute,
	)

	first, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("first sweep = %v, want no error", err)
	}
	if first.Created != 2 {
		t.Fatalf("created = %d, want 2", first.Created)
	}

	// A third project joins; the two existing ones must be left alone.
	projects.setRefs([]port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
		{UID: "project-2", SubStage: model.StageFormationOnHold},
		{UID: "project-3", SubStage: model.StageFormationEngaged},
	})

	second, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("second sweep = %v, want no error", err)
	}
	if second.Swept != 3 {
		t.Errorf("swept = %d, want 3", second.Swept)
	}
	if second.Created != 1 {
		t.Errorf("created = %d, want 1 — only the new project needed a checklist", second.Created)
	}
	if second.Failed != 0 {
		t.Errorf("failed = %d, want 0", second.Failed)
	}
}

// No published template is the state a freshly deployed environment is in, and
// it is a condition of the sweep rather than of any one project. It is looked up
// and reported once, and — the part worth pinning — lifecycles are still synced,
// because moving a checklist that already exists needs no template.
func TestReconcileSyncsLifecyclesWhenNoTemplateIsPublished(t *testing.T) {
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
		formations,
		NewExpander(NewTemplateSelector(templates), uow, projects),
		NewLifecycler(formations),
		time.Minute,
	)

	// One project is already at a stage that completes its checklist, so there
	// is lifecycle work to do that must survive the missing template.
	if _, err := formations.Create(ctx, &model.Formation{
		ProjectUID: "project-2",
		Lifecycle:  model.LifecycleLive,
		Revision:   1,
	}); err != nil {
		t.Fatalf("seeding a formation = %v", err)
	}
	projects.setRefs([]port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
		{UID: "project-2", SubStage: model.StageActive},
	})

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error — the loop must survive this", err)
	}
	if report.Created != 0 {
		t.Errorf("created = %d, want 0 — nothing can be created with no template", report.Created)
	}
	if report.LifecyclesMoved != 1 {
		t.Errorf("lifecycles_moved = %d, want 1 — a missing template must not stop lifecycle sync", report.LifecyclesMoved)
	}
	// Counted as blocked, not failed: one condition holds up the forming
	// project, and it is logged once rather than once per project.
	if report.Blocked != 1 {
		t.Errorf("blocked = %d, want 1", report.Blocked)
	}
	if report.Failed != 0 {
		t.Errorf("failed = %d, want 0 — a shared condition is not a per-project failure", report.Failed)
	}
}

// Blocked means "should have had a checklist created and could not". A forming
// project that already has one is waiting on nothing, so a missing template must
// not count it — otherwise a fresh environment reports every project on the
// platform as blocked and the number stops being usable as a signal.
func TestAMissingTemplateDoesNotBlockProjectsThatAlreadyHaveChecklists(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{}

	// No fixture, so no published template: the sweep-wide condition under test.
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	templates := mock.NewTemplateRepository()
	uow := mock.NewUnitOfWork(formations, items, mock.NewActivityRepository(), templates)

	r := NewReconciler(
		projects,
		formations,
		NewExpander(NewTemplateSelector(templates), uow, projects),
		NewLifecycler(formations),
		time.Minute,
	)

	// Two forming projects, one of which already has its checklist.
	if _, err := formations.Create(ctx, &model.Formation{
		ProjectUID: "has-one",
		Lifecycle:  model.LifecycleLive,
		Revision:   1,
	}); err != nil {
		t.Fatalf("seeding a formation = %v", err)
	}
	projects.setRefs([]port.ProjectRef{
		{UID: "has-one", SubStage: model.StageFormationEngaged},
		{UID: "needs-one", SubStage: model.StageFormationEngaged},
	})

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error", err)
	}
	if report.Blocked != 1 {
		t.Errorf("blocked = %d, want 1 — only the project with no checklist is held up", report.Blocked)
	}
	if report.Created != 0 {
		t.Errorf("created = %d, want 0", report.Created)
	}
	if report.Failed != 0 {
		t.Errorf("failed = %d, want 0", report.Failed)
	}
}

// A failure that belongs to one project must not end the sweep: the rest still
// need their checklists.
func TestReconcileContinuesPastOneProjectsFailure(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "", SubStage: model.StageFormationEngaged},
		{UID: "project-2", SubStage: model.StageFormationEngaged},
	}}
	r, f := newReconciler(t, projects)

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error", err)
	}
	if report.Swept != 2 {
		t.Errorf("swept = %d, want 2", report.Swept)
	}
	// The blank UID is refused; the project behind it is still served.
	if report.Failed != 1 {
		t.Errorf("failed = %d, want 1", report.Failed)
	}
	if report.Created != 1 {
		t.Errorf("created = %d, want 1 — the sweep abandoned the project after the failing one", report.Created)
	}
	if _, err := f.formations.GetByProject(ctx, "project-2"); err != nil {
		t.Errorf("project-2 has no checklist: %v", err)
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
	r := NewReconciler(nil, f.formations, f.expander, NewLifecycler(f.formations), time.Minute)

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

// failingUpdateLifecycle answers UpdateLifecycle with an error and otherwise
// behaves normally, so a sweep can be driven with only the lifecycle write
// broken.
type failingUpdateLifecycle struct {
	port.FormationRepository
	err error
}

func (f *failingUpdateLifecycle) UpdateLifecycle(
	_ context.Context, _ uuid.UUID, _ model.Lifecycle, _ int64,
) (*model.Formation, error) {
	return nil, f.err
}

// A lifecycle that could not be moved has to reach the summary. The sweep
// deliberately continues past one, but continuing quietly is what made a sweep
// where every single move failed still report failed=0 — an operator reading
// that line saw a clean run and had no reason to go looking for the error logs
// underneath it.
func TestALifecycleThatCannotBeMovedIsCounted(t *testing.T) {
	ctx := context.Background()

	// Active, so the checklist that exists has a move to make: without one
	// there is nothing to fail.
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageActive},
	}}
	f := newExpansionFixture(t, twoItemSections(), projects)
	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("ExpandFor() = %v, want no error", err)
	}

	broken := &failingUpdateLifecycle{FormationRepository: f.formations, err: errors.New("connection reset")}
	r := NewReconciler(projects, f.formations, f.expander, NewLifecycler(broken), time.Minute)

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error — one project must not end the sweep", err)
	}
	if report.Failed != 1 {
		t.Errorf("failed = %d, want 1 — a lifecycle that could not move is a failure worth reporting", report.Failed)
	}
	if report.LifecyclesMoved != 0 {
		t.Errorf("lifecycles_moved = %d, want 0 — nothing moved", report.LifecyclesMoved)
	}
	// The sweep still visited it, which is what distinguishes a counted failure
	// from a project that was never reached.
	if report.Swept != 1 {
		t.Errorf("swept = %d, want 1", report.Swept)
	}
}

// With both sweep-wide reads gone — the existing checklists and the template —
// every forming project is absent from an empty set, so counting them as blocked
// would assert they need a checklist and cannot have one. The sweep does not know
// that: it could not read which ones already have one. Blocked has to stay a
// claim the sweep can support, with the unknown case reported separately.
func TestBothSweepReadsFailingIsDegradedRatherThanBlocked(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
		{UID: "project-2", SubStage: model.StageFormationEngaged},
	}}
	f := newExpansionFixture(t, twoItemSections(), projects)

	// project-1 already has a checklist, so it is one of the projects a blocked
	// count would be wrong about.
	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("ExpandFor() = %v, want no error", err)
	}

	boom := errors.New("connection reset")
	brokenFormations := &failingListFormations{FormationRepository: f.formations, err: boom}
	brokenExpander := NewExpander(
		NewTemplateSelector(&failingTemplates{TemplateRepository: f.templates, err: boom}), f.uow, projects)

	r := NewReconciler(projects, brokenFormations, brokenExpander, NewLifecycler(f.formations), time.Minute)

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error — neither read is needed to sync a lifecycle", err)
	}
	if report.Blocked != 0 {
		t.Errorf("blocked = %d, want 0 — the sweep cannot tell which projects need a checklist", report.Blocked)
	}
	if report.Degraded != 2 {
		t.Errorf("degraded = %d, want 2 — both projects went unresolved", report.Degraded)
	}
	if report.Created != 0 {
		t.Errorf("created = %d, want 0 — there is no template to create from", report.Created)
	}
}

// The counterpart: with the checklist set readable and only the template gone,
// absence is known, so blocked is a claim the sweep can support — and a project
// that already has a checklist is still not counted.
func TestOnlyTheTemplateFailingStillReportsBlocked(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
		{UID: "project-2", SubStage: model.StageFormationEngaged},
	}}
	f := newExpansionFixture(t, twoItemSections(), projects)
	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("ExpandFor() = %v, want no error", err)
	}

	brokenExpander := NewExpander(
		NewTemplateSelector(&failingTemplates{TemplateRepository: f.templates, err: errors.New("connection reset")}),
		f.uow, projects)
	r := NewReconciler(projects, f.formations, brokenExpander, NewLifecycler(f.formations), time.Minute)

	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error", err)
	}
	if report.Blocked != 1 {
		t.Errorf("blocked = %d, want 1 — only project-2 needs a checklist and cannot have one", report.Blocked)
	}
	if report.Degraded != 0 {
		t.Errorf("degraded = %d, want 0 — the checklist set was readable", report.Degraded)
	}
}
