// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

func liveFormation() *model.Formation {
	return &model.Formation{ProjectUID: "project-1", Lifecycle: model.LifecycleLive}
}

// The five counts are published as fields because the queue sorts on them, and
// the items themselves are not in the document for the browser to count.
func TestProjectionCountsEveryFiveStatuses(t *testing.T) {
	items := []*model.Item{
		{ItemKey: "a", Status: model.StatusNotStarted},
		{ItemKey: "b", Status: model.StatusInProgress},
		{ItemKey: "c", Status: model.StatusInProgress},
		{ItemKey: "d", Status: model.StatusBlocked, Title: "Blocked one"},
		{ItemKey: "f", Status: model.StatusDone},
		{ItemKey: "g", Status: model.StatusSkipped},
	}
	doc := buildProjection(liveFormation(), items, port.ProjectRef{}, "A Project", "", nil, time.Now())

	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"not_started", doc.NotStarted, 1},
		{"in_progress", doc.InProgress, 2},
		{"blocked", doc.Blocked, 1},
		{"done", doc.Done, 1},
		{"skipped", doc.Skipped, 1},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// Readiness is published in two halves under two names, and this is the case
// that makes the split necessary: gates cleared, no announcement date. The queue
// needs to show that row as gates-cleared, while full readiness is false.
func TestGatesClearedAndIsActivatingAreSeparate(t *testing.T) {
	gatedDone := []*model.Item{
		{ItemKey: "gate", Gate: true, Status: model.StatusDone},
		{ItemKey: "other", Status: model.StatusInProgress},
	}

	withoutDate := buildProjection(liveFormation(), gatedDone, port.ProjectRef{}, "A Project", "", nil, time.Now())
	if !withoutDate.GatesCleared {
		t.Error("gates_cleared = false with every gating item done, want true")
	}
	if withoutDate.IsActivating {
		t.Error("is_activating = true with no announcement date, want false — " +
			"full readiness needs the date as well")
	}

	withDate := buildProjection(liveFormation(), gatedDone, port.ProjectRef{}, "A Project", "2026-12-01", nil, time.Now())
	if !withDate.IsActivating {
		t.Error("is_activating = false with gates cleared and a date set, want true")
	}
}

// A checklist with no gating items must not report itself ready on a vacuous
// truth, on either field.
func TestNoGatingItemsIsNeitherClearedNorActivating(t *testing.T) {
	doc := buildProjection(liveFormation(),
		[]*model.Item{{ItemKey: "a", Status: model.StatusDone}},
		port.ProjectRef{}, "A Project", "2026-12-01", nil, time.Now())

	if doc.GatesCleared {
		t.Error("gates_cleared = true with zero gating items, want false")
	}
	if doc.IsActivating {
		t.Error("is_activating = true with zero gating items, want false")
	}
}

// Only done satisfies a gate. A skipped gating item was excused, not completed,
// so it must not clear the way to Active — and nor must anything short of done.
func TestOnlyDoneSatisfiesAGate(t *testing.T) {
	for _, status := range []model.ItemStatus{
		model.StatusNotStarted, model.StatusInProgress, model.StatusBlocked, model.StatusSkipped,
	} {
		doc := buildProjection(liveFormation(),
			[]*model.Item{{ItemKey: "gate", Gate: true, Status: status}},
			port.ProjectRef{}, "A Project", "2026-12-01", nil, time.Now())

		if doc.GatesCleared {
			t.Errorf("gates_cleared = true with its only gating item %q, want false", status)
		}
		if doc.IsActivating {
			t.Errorf("is_activating = true with its only gating item %q, want false", status)
		}
	}
}

// No stored three-way type. The browser derives Foundation / Project / Child
// project from these two signals, because indenting a row depends on whether the
// parent is also in the result set — which the browser knows and a publisher
// looking at one checklist does not.
func TestProjectionPublishesTheSignalsAndNotAType(t *testing.T) {
	doc := buildProjection(liveFormation(), nil,
		port.ProjectRef{
			UID: "project-1", Slug: "a-project", IsFoundation: true,
			ParentUID: "parent-1", SubStage: model.StageFormationEngaged,
		},
		"A Project", "", nil, time.Now())

	if !doc.IsFoundation {
		t.Error("is_foundation = false, want true")
	}
	if doc.ParentUID != "parent-1" {
		t.Errorf("parent_uid = %q, want parent-1", doc.ParentUID)
	}
	if doc.SubStage != model.StageFormationEngaged {
		t.Errorf("sub_stage = %q, want the compound formation stage", doc.SubStage)
	}
}

// "Mine" matches on assignees, so the set has to be distinct — one person
// usually holds several items and the filter asks only whether they hold any.
func TestAssigneesAreDistinctAndExcludeTheUnassigned(t *testing.T) {
	doc := buildProjection(liveFormation(), []*model.Item{
		{ItemKey: "a", Assignee: "person-one"},
		{ItemKey: "b", Assignee: "person-one"},
		{ItemKey: "c", Assignee: "person-two"},
		{ItemKey: "d", Assignee: ""},
	}, port.ProjectRef{}, "A Project", "", nil, time.Now())

	if len(doc.Assignees) != 2 {
		t.Fatalf("assignees = %v, want two distinct entries", doc.Assignees)
	}
	for _, a := range doc.Assignees {
		if a == "" {
			t.Error("assignees contains an empty entry, which would match an unassigned filter")
		}
	}
}

// Republishing an unchanged checklist must produce an unchanged document, or
// every sweep writes to the index for nothing. Items arrive ordered by section
// and position, but which of them are blocked changes with status, so the arrays
// are sorted rather than left in arrival order.
func TestRepublishingIsStableForAnUnchangedChecklist(t *testing.T) {
	items := []*model.Item{
		{ItemKey: "b", Title: "Beta", Status: model.StatusBlocked, Assignee: "person-two"},
		{ItemKey: "a", Title: "Alpha", Status: model.StatusBlocked, Assignee: "person-one"},
	}
	now := time.Now()
	first := buildProjection(liveFormation(), items, port.ProjectRef{}, "A Project", "", nil, now)

	reordered := []*model.Item{items[1], items[0]}
	second := buildProjection(liveFormation(), reordered, port.ProjectRef{}, "A Project", "", nil, now)

	if len(first.BlockedItemTitles) != len(second.BlockedItemTitles) {
		t.Fatalf("titles = %v and %v, want the same", first.BlockedItemTitles, second.BlockedItemTitles)
	}
	for i := range first.BlockedItemTitles {
		if first.BlockedItemTitles[i] != second.BlockedItemTitles[i] {
			t.Errorf("titles differ only in order: %v vs %v",
				first.BlockedItemTitles, second.BlockedItemTitles)
		}
	}
	for i := range first.Assignees {
		if first.Assignees[i] != second.Assignees[i] {
			t.Errorf("assignees differ only in order: %v vs %v", first.Assignees, second.Assignees)
		}
	}
}

// A Confidential project is not filtered out of the queue. It is missing from
// someone's results because they hold no access to it, not because the query
// excludes a stage — so the row is published like any other, and the access
// relation is what withholds it.
func TestAConfidentialProjectIsPublishedLikeAnyOther(t *testing.T) {
	ctx := context.Background()
	projects := mock.NewProjectReader()
	projects.SetName("project-1", "A Confidential Project")
	projects.SetFormingProjects([]port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationConfidential},
	})
	r, _, publisher := newReconcilerWithIndex(t, projects)

	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("sweep = %v, want no error", err)
	}

	doc := publisher.Latest("project-1")
	if doc == nil {
		t.Fatal("a confidential project published no row — it must be withheld by the access " +
			"relation, not by being absent from the index")
	}
	if doc.SubStage != model.StageFormationConfidential {
		t.Errorf("sub_stage = %q, want the confidential stage carried through", doc.SubStage)
	}
	if doc.AccessRelation != "auditor" {
		t.Errorf("access relation = %q, want auditor — this is what withholds the row", doc.AccessRelation)
	}
}

// One row per checklist per sweep, and no request per row. The queue is one
// search, so a projection that fanned out per item would be the pattern this
// design exists to avoid.
func TestTheSweepPublishesOneRowPerChecklist(t *testing.T) {
	ctx := context.Background()
	projects := mock.NewProjectReader()
	projects.SetFormingProjects([]port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
		{UID: "project-2", SubStage: model.StageFormationExploratory},
	})
	r, _, publisher := newReconcilerWithIndex(t, projects)

	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("sweep = %v, want no error", err)
	}

	// Two projects, two items each from the fixture template, so a per-item
	// publisher would have sent four.
	if got := publisher.Count(); got != 2 {
		t.Errorf("published %d rows for 2 checklists, want 2", got)
	}
}

// An unpublished row is repaired by the ordinary sweep, with no bespoke tooling:
// the projection is republished every tick whether or not anything changed.
func TestALostRowIsRepublishedByTheNextSweep(t *testing.T) {
	ctx := context.Background()
	projects := mock.NewProjectReader()
	projects.SetFormingProjects([]port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
	})
	r, _, publisher := newReconcilerWithIndex(t, projects)

	// The first sweep's publish fails, standing in for a lost message.
	publisher.SetError(errors.New("indexer unreachable"))
	report, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("sweep with a failing publisher = %v, want no error — a stale queue must not "+
			"stop checklists being created", err)
	}
	if report.Created != 1 {
		t.Errorf("created = %d, want 1 despite the publish failing", report.Created)
	}
	if report.ProjectionFailed != 1 {
		t.Errorf("projection_failed = %d, want 1", report.ProjectionFailed)
	}
	if publisher.Count() != 0 {
		t.Fatalf("published %d rows, want 0", publisher.Count())
	}

	// The next sweep creates nothing and republishes anyway, which is what makes
	// the repair path the ordinary path.
	publisher.SetError(nil)
	report, err = r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("second sweep = %v, want no error", err)
	}
	if report.Created != 0 {
		t.Errorf("created = %d on the second sweep, want 0", report.Created)
	}
	if report.Projected != 1 {
		t.Errorf("projected = %d, want 1 — the row must be republished with nothing else to do",
			report.Projected)
	}
	if publisher.Latest("project-1") == nil {
		t.Error("the row is still missing after a sweep that could publish")
	}
}

// Every ItemProjection field maps to its Item counterpart, and AccessRelation
// is set unconditionally to the checklist's own relation — never viewer,
// regardless of the item's own status, gate, or content: an item is indexed
// the same way the checklist is.
func TestBuildItemProjectionsMapsEveryField(t *testing.T) {
	itemUID := uuid.New()
	formationUID := uuid.New()
	due := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	formation := &model.Formation{UID: formationUID, ProjectUID: "project-1", Lifecycle: model.LifecycleLive}
	items := []*model.Item{
		{
			UID:          itemUID,
			FormationUID: formationUID,
			ItemKey:      "create_mailing_list",
			Title:        "Create mailing list",
			StatusSource: model.SourcePlatform,
			Status:       model.StatusInProgress,
			Gate:         true,
			DueDate:      &due,
			OwnerTeam:    "it",
			ActionLink:   "https://groups.io/g/create",
			Assignee:     "jdoe",
			SubItems: []model.SubItem{
				{Key: "groupsio-request", Title: "Request Groups.io space", Status: model.StatusDone},
			},
		},
	}

	got := buildItemProjections(formation, items, port.ProjectRef{Slug: "a-project"}, "A Project")
	if len(got) != 1 {
		t.Fatalf("buildItemProjections() = %d projections, want 1", len(got))
	}
	doc := got[0]

	if doc.ItemUID != itemUID.String() {
		t.Errorf("item_uid = %q, want %q", doc.ItemUID, itemUID.String())
	}
	if doc.FormationUID != formationUID.String() {
		t.Errorf("formation_uid = %q, want %q", doc.FormationUID, formationUID.String())
	}
	if doc.ProjectUID != "project-1" {
		t.Errorf("project_uid = %q, want project-1 — resolved through the formation", doc.ProjectUID)
	}
	if doc.ProjectName != "A Project" {
		t.Errorf("project_name = %q, want A Project — a Pending Actions row's badge needs it without a second read", doc.ProjectName)
	}
	if doc.ProjectSlug != "a-project" {
		t.Errorf("project_slug = %q, want a-project", doc.ProjectSlug)
	}
	if doc.StatusSource != string(model.SourcePlatform) {
		t.Errorf("status_source = %q, want %q", doc.StatusSource, model.SourcePlatform)
	}
	if doc.Lifecycle != string(model.LifecycleLive) {
		t.Errorf("lifecycle = %q, want %q", doc.Lifecycle, model.LifecycleLive)
	}
	if doc.ItemKey != "create_mailing_list" {
		t.Errorf("item_key = %q, want create_mailing_list", doc.ItemKey)
	}
	if doc.Title != "Create mailing list" {
		t.Errorf("title = %q, want Create mailing list", doc.Title)
	}
	if doc.Status != string(model.StatusInProgress) {
		t.Errorf("status = %q, want %q", doc.Status, model.StatusInProgress)
	}
	if !doc.Gate {
		t.Error("gate = false, want true")
	}
	if doc.DueDate != "2026-10-01" {
		t.Errorf("due_date = %q, want 2026-10-01", doc.DueDate)
	}
	if doc.OwnerTeam != "it" {
		t.Errorf("owner_team = %q, want it", doc.OwnerTeam)
	}
	if doc.ActionLink != "https://groups.io/g/create" {
		t.Errorf("action_link = %q, want the template's link", doc.ActionLink)
	}
	if doc.Assignee != "jdoe" {
		t.Errorf("assignee = %q, want jdoe", doc.Assignee)
	}
	if len(doc.SubItems) != 1 || doc.SubItems[0].Key != "groupsio-request" ||
		doc.SubItems[0].Status != string(model.StatusDone) {
		t.Errorf("sub_items = %+v, want the one sub-item carried through", doc.SubItems)
	}
	if doc.AccessRelation != formationAccessRelation {
		t.Errorf("access_relation = %q, want %q — never viewer", doc.AccessRelation, formationAccessRelation)
	}
}

// due_date, owner_team and action_link are omitted (empty string on the
// projection, which PublishItem later omits from the wire document) rather
// than carrying a zero value, matching the checklist projection's own
// omitEmpty convention.
func TestBuildItemProjectionsOmitsUnsetOptionalFields(t *testing.T) {
	formation := &model.Formation{UID: uuid.New(), ProjectUID: "project-1"}
	items := []*model.Item{{UID: uuid.New(), FormationUID: formation.UID, Title: "Bare item"}}

	doc := buildItemProjections(formation, items, port.ProjectRef{}, "")[0]

	if doc.DueDate != "" {
		t.Errorf("due_date = %q, want empty when unset", doc.DueDate)
	}
	if doc.OwnerTeam != "" {
		t.Errorf("owner_team = %q, want empty when unset", doc.OwnerTeam)
	}
	if doc.ActionLink != "" {
		t.Errorf("action_link = %q, want empty when unset", doc.ActionLink)
	}
}

// An item's Assignee attribute is absent, not empty-stringed, when the item
// carries no assignee — and reassigning an item changes which projection
// carries its assignee, with no residue on the previous holder's side.
func TestBuildItemProjectionsAssigneePresenceTracksTheItem(t *testing.T) {
	formation := &model.Formation{UID: uuid.New(), ProjectUID: "project-1"}
	unassigned := &model.Item{UID: uuid.New(), FormationUID: formation.UID, Title: "Unassigned"}
	assigned := &model.Item{UID: uuid.New(), FormationUID: formation.UID, Title: "Assigned", Assignee: "jdoe"}

	docs := buildItemProjections(formation, []*model.Item{unassigned, assigned}, port.ProjectRef{}, "")

	if docs[0].Assignee != "" {
		t.Errorf("assignee = %q, want empty for an unassigned item", docs[0].Assignee)
	}
	if docs[1].Assignee != "jdoe" {
		t.Errorf("assignee = %q, want jdoe", docs[1].Assignee)
	}

	// Reassign: the same item, a different holder.
	assigned.Assignee = "asmith"
	reassigned := buildItemProjections(formation, []*model.Item{assigned}, port.ProjectRef{}, "")[0]
	if reassigned.Assignee != "asmith" {
		t.Errorf("assignee after reassignment = %q, want asmith", reassigned.Assignee)
	}
}

// A Pending Actions row's "N of M" summary and owner-team label need the
// item's own owner_team, full sub_items list, and action_link on the
// projection — not just a count or a boolean — so the row can render without
// a second read against the checklist.
func TestBuildItemProjectionsCarriesEveryRowFieldTheDrawerDoesNotOwn(t *testing.T) {
	formation := &model.Formation{UID: uuid.New(), ProjectUID: "project-1"}
	items := []*model.Item{{
		UID:          uuid.New(),
		FormationUID: formation.UID,
		Title:        "Create mailing list",
		OwnerTeam:    "it",
		ActionLink:   "https://groups.io/g/create",
		SubItems: []model.SubItem{
			{Key: "groupsio-request", Title: "Request Groups.io space", Status: model.StatusDone},
			{Key: "groupsio-configure", Title: "Configure lists", Status: model.StatusNotStarted},
		},
	}}

	doc := buildItemProjections(formation, items, port.ProjectRef{}, "")[0]

	if doc.OwnerTeam != "it" {
		t.Errorf("owner_team = %q, want it — the row's \"handled by IT\" label needs it", doc.OwnerTeam)
	}
	if doc.ActionLink != "https://groups.io/g/create" {
		t.Errorf("action_link = %q, want the item's own link — the row's \"open link\" CTA needs it",
			doc.ActionLink)
	}
	if len(doc.SubItems) != 2 {
		t.Fatalf("sub_items = %d entries, want 2 — the row's \"N of M\" summary needs the full "+
			"list, not a count", len(doc.SubItems))
	}
	for i, want := range []port.ItemProjectionSubItem{
		{Key: "groupsio-request", Title: "Request Groups.io space", Status: string(model.StatusDone)},
		{Key: "groupsio-configure", Title: "Configure lists", Status: string(model.StatusNotStarted)},
	} {
		if doc.SubItems[i] != want {
			t.Errorf("sub_items[%d] = %+v, want %+v", i, doc.SubItems[i], want)
		}
	}
}

// ItemProjection must never gain a field for notes, skip reason, or resolved
// ref — those are drawer-only detail, read from the checklist directly, never
// from this index. Enumerated by reflection rather than by field access,
// because there is no field to read yet: if one is ever added, this test
// fails and forces the addition to be a conscious decision rather than a
// silent leak.
func TestItemProjectionNeverGainsADrawerOnlyField(t *testing.T) {
	forbidden := map[string]bool{"Note": true, "SkipReason": true, "ResolvedRef": true, "EvidenceLink": true}

	fields := reflect.TypeOf(port.ItemProjection{})
	for i := 0; i < fields.NumField(); i++ {
		name := fields.Field(i).Name
		if forbidden[name] {
			t.Errorf("ItemProjection carries a %q field; notes/skip-reason/resolved-ref/evidence-link "+
				"are drawer-only detail and must never reach this index", name)
		}
	}
}

// AccessRelation is auditor for every item, unconditionally — regardless of
// status, gate, or any other content. There is no code path that produces
// viewer or an empty value.
func TestBuildItemProjectionsAccessRelationIsAlwaysAuditor(t *testing.T) {
	formation := &model.Formation{UID: uuid.New(), ProjectUID: "project-1"}
	items := []*model.Item{
		{UID: uuid.New(), FormationUID: formation.UID, Status: model.StatusNotStarted, Gate: false},
		{UID: uuid.New(), FormationUID: formation.UID, Status: model.StatusBlocked, Gate: true},
		{UID: uuid.New(), FormationUID: formation.UID, Status: model.StatusDone, Gate: true, Assignee: "jdoe"},
		{UID: uuid.New(), FormationUID: formation.UID, Status: model.StatusSkipped, Gate: false},
	}

	for _, doc := range buildItemProjections(formation, items, port.ProjectRef{}, "") {
		if doc.AccessRelation != formationAccessRelation {
			t.Errorf("access_relation = %q for status %q, gate %v; want %q unconditionally",
				doc.AccessRelation, doc.Status, doc.Gate, formationAccessRelation)
		}
	}
}

// Refresh publishes one item document per item, alongside the checklist
// document it already publishes — not a fan-out per project, a fan-out per
// item within the one project this call already read.
func TestRefreshPublishesOneItemDocumentPerItem(t *testing.T) {
	ctx := context.Background()
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	publisher := mock.NewIndexerPublisher()
	projector := NewProjector(formations, items, mock.NewProjectReader(), publisher)

	formation, err := formations.Create(ctx, &model.Formation{ProjectUID: "project-1"})
	if err != nil {
		t.Fatalf("seeding formation = %v", err)
	}
	if _, err := items.InsertMany(ctx, []*model.Item{
		{FormationUID: formation.UID, ItemKey: "a", Title: "Item A"},
		{FormationUID: formation.UID, ItemKey: "b", Title: "Item B"},
		{FormationUID: formation.UID, ItemKey: "c", Title: "Item C"},
	}); err != nil {
		t.Fatalf("seeding items = %v", err)
	}

	published, err := projector.Refresh(ctx, port.ProjectRef{UID: "project-1"}, FirstPublish)
	if err != nil {
		t.Fatalf("Refresh() = %v, want no error", err)
	}
	if !published {
		t.Fatal("published = false, want true")
	}
	if got := publisher.ItemCount(); got != 3 {
		t.Errorf("published %d item documents for 3 items, want 3", got)
	}
	if got := publisher.Count(); got != 1 {
		t.Errorf("published %d checklist documents, want 1 — the checklist document is unchanged",
			got)
	}
}

// A checklist is never deleted when its project leaves formation — it becomes
// completed (read-only history) or frozen, and the sweep keeps republishing
// it. So its items keep being republished too, at whatever status the
// transition left them, and Lifecycle.Mutable() means they can never move
// again. The item documents have to carry that lifecycle for a consumer to
// tell "still outstanding" from "outstanding when the music stopped".
// Both values, because false is the one that carries information: it marks an
// item an auditor-only assignee can act on, so a row reading only the item's
// half of the question still gets a usable answer.
func TestBuildItemProjectionsCarriesEachItemsRequiresWriter(t *testing.T) {
	formation := &model.Formation{UID: uuid.New(), ProjectUID: "project-1"}
	items := []*model.Item{
		{UID: uuid.New(), FormationUID: formation.UID, ItemKey: "needs-writer", RequiresWriter: true},
		{UID: uuid.New(), FormationUID: formation.UID, ItemKey: "auditor-can-do", RequiresWriter: false},
	}

	docs := buildItemProjections(formation, items, port.ProjectRef{}, "")

	byKey := map[string]bool{}
	for _, doc := range docs {
		byKey[doc.ItemKey] = doc.RequiresWriter
	}
	if !byKey["needs-writer"] {
		t.Error("requires_writer = false for the item that needs writer")
	}
	if byKey["auditor-can-do"] {
		t.Error("requires_writer = true for the item an auditor can act on")
	}
}

func TestBuildItemProjectionsCarriesANonLiveLifecycle(t *testing.T) {
	for _, lifecycle := range []model.Lifecycle{model.LifecycleCompleted, model.LifecycleFrozen} {
		formation := &model.Formation{UID: uuid.New(), ProjectUID: "project-1", Lifecycle: lifecycle}
		items := []*model.Item{{
			UID:          uuid.New(),
			FormationUID: formation.UID,
			Title:        "Left mid-flight",
			Status:       model.StatusInProgress,
			Assignee:     "jdoe",
		}}

		doc := buildItemProjections(formation, items, port.ProjectRef{}, "")[0]

		if doc.Lifecycle != string(lifecycle) {
			t.Errorf("lifecycle = %q, want %q", doc.Lifecycle, lifecycle)
		}
		if lifecycle.Mutable() {
			t.Errorf("%q reports mutable; this test's premise is that it is not", lifecycle)
		}
	}
}

// Refresh resolves the project's name and slug once, for the checklist
// document — this asserts the item documents it publishes in the same call
// carry those same two facts, not a second, independent resolution (there is
// none: ProjectReader.Name is stubbed to fail the test if called more than
// the checklist publish already calls it for).
func TestRefreshCarriesProjectNameAndSlugOntoItemDocuments(t *testing.T) {
	ctx := context.Background()
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	publisher := mock.NewIndexerPublisher()
	projects := mock.NewProjectReader()
	projects.SetName("project-1", "A Project")
	projector := NewProjector(formations, items, projects, publisher)

	formation, err := formations.Create(ctx, &model.Formation{ProjectUID: "project-1"})
	if err != nil {
		t.Fatalf("seeding formation = %v", err)
	}
	if _, err := items.InsertMany(ctx, []*model.Item{
		{FormationUID: formation.UID, ItemKey: "a", Title: "Item A"},
	}); err != nil {
		t.Fatalf("seeding items = %v", err)
	}

	if _, err := projector.Refresh(ctx, port.ProjectRef{UID: "project-1", Slug: "a-project"}, FirstPublish); err != nil {
		t.Fatalf("Refresh() = %v, want no error", err)
	}

	published := publisher.ItemPublished()
	if len(published) != 1 {
		t.Fatalf("published %d item documents, want 1", len(published))
	}
	if published[0].ProjectName != "A Project" {
		t.Errorf("item's project_name = %q, want A Project", published[0].ProjectName)
	}
	if published[0].ProjectSlug != "a-project" {
		t.Errorf("item's project_slug = %q, want a-project", published[0].ProjectSlug)
	}
}

// Refresh's two return values feed two different sweep counters: an error is
// a failed projection, a false is a project holding no checklist to publish.
// A checklist whose row published but whose item rows all failed belongs to
// the first, not the second — reporting it as a bare false would count a
// stale index as neither a success nor a failure.
func TestRefreshReturnsAnErrorWhenNoItemDocumentLands(t *testing.T) {
	ctx := context.Background()
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	publisher := mock.NewIndexerPublisher()
	projector := NewProjector(formations, items, mock.NewProjectReader(), publisher)

	formation, err := formations.Create(ctx, &model.Formation{ProjectUID: "project-1"})
	if err != nil {
		t.Fatalf("seeding formation = %v", err)
	}
	if _, err := items.InsertMany(ctx, []*model.Item{
		{FormationUID: formation.UID, ItemKey: "a", Title: "Item A"},
	}); err != nil {
		t.Fatalf("seeding items = %v", err)
	}

	// The checklist publish still succeeds; only the item batch fails.
	publisher.SetItemsError(errors.New("indexer unreachable for items"))

	published, err := projector.Refresh(ctx, port.ProjectRef{UID: "project-1"}, FirstPublish)
	if err == nil {
		t.Fatal("Refresh() = nil error, want one — the sweep counts a stale index only when told")
	}
	if !strings.Contains(err.Error(), "indexer unreachable for items") {
		t.Errorf("Refresh() = %v, want the underlying publish failure wrapped", err)
	}
	if published {
		t.Error("published = true, want false — no item document landed for this checklist")
	}
	// The checklist row itself still published: best-effort item publishing
	// must not undo a publish that already succeeded.
	if got := publisher.Count(); got != 1 {
		t.Errorf("published %d checklist documents, want 1 despite the item failure", got)
	}
}

// An item's document is repaired by the ordinary sweep, with no bespoke
// tooling — the same guarantee TestALostRowIsRepublishedByTheNextSweep makes
// for the checklist document, applied to the per-item documents this sweep
// now also publishes.
func TestALostItemRowIsRepublishedByTheNextSweep(t *testing.T) {
	ctx := context.Background()
	projects := mock.NewProjectReader()
	projects.SetFormingProjects([]port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
	})
	r, _, publisher := newReconcilerWithIndex(t, projects)

	// The first sweep's publishes fail, standing in for a lost message.
	publisher.SetError(errors.New("indexer unreachable"))
	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("sweep with a failing publisher = %v, want no error", err)
	}
	if got := publisher.ItemCount(); got != 0 {
		t.Fatalf("published %d item rows, want 0", got)
	}

	// The next sweep republishes anyway, which is what makes the repair path
	// the ordinary path.
	publisher.SetError(nil)
	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("second sweep = %v, want no error", err)
	}
	// The fixture's template carries three items (twoItemSections).
	if got := publisher.ItemCount(); got != 3 {
		t.Errorf("published %d item rows on the second sweep, want 3", got)
	}
}

// The name falls back to the slug rather than dropping the row. A row absent
// because a name could not be read is indistinguishable from a project the
// viewer may not see, which is the one failure this screen must not have.
func TestAnUnresolvableNameFallsBackToTheSlugRatherThanDroppingTheRow(t *testing.T) {
	ctx := context.Background()
	projects := mock.NewProjectReader()
	// No SetName, so Name answers ErrNotFound.
	projects.SetFormingProjects([]port.ProjectRef{
		{UID: "project-1", Slug: "a-project", SubStage: model.StageFormationEngaged},
	})
	r, _, publisher := newReconcilerWithIndex(t, projects)

	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("sweep = %v, want no error", err)
	}

	doc := publisher.Latest("project-1")
	if doc == nil {
		t.Fatal("no row published when the name could not be resolved")
	}
	if doc.ProjectName != "a-project" {
		t.Errorf("project_name = %q, want the slug as a fallback", doc.ProjectName)
	}
}

// The lifecycle is published, and it is published after the sync rather than
// before: a queue a tick behind on the transition someone is watching for is the
// reason the order in finishProject is fixed.
func TestTheRowCarriesTheLifecycleItWasMovedToThisSweep(t *testing.T) {
	ctx := context.Background()
	projects := mock.NewProjectReader()
	projects.SetFormingProjects([]port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
	})
	r, _, publisher := newReconcilerWithIndex(t, projects)

	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("create sweep = %v", err)
	}
	if got := publisher.Latest("project-1").Lifecycle; got != string(model.LifecycleLive) {
		t.Errorf("lifecycle = %q, want live", got)
	}

	// The project goes Active, which completes its checklist.
	projects.SetFormingProjects(nil)
	projects.SetProjectsByUID([]port.ProjectRef{{UID: "project-1", SubStage: model.StageActive}})

	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("sweep after going active = %v", err)
	}
	if got := publisher.Latest("project-1").Lifecycle; got != string(model.LifecycleCompleted) {
		t.Errorf("lifecycle = %q, want completed in the same sweep that moved it", got)
	}
}

// StalledCount is nil when no items are assigned — nil means "nothing to
// chase", which is a different signal from zero ("assigned work exists, none
// of it is overdue"). The queue must not render a count cell for unassigned
// checklists, and nil is the flag it checks.
func TestStalledCountIsNilWhenNothingIsAssigned(t *testing.T) {
	items := []*model.Item{
		{ItemKey: "a", Status: model.StatusNotStarted},
		{ItemKey: "b", Status: model.StatusInProgress},
	}
	doc := buildProjection(liveFormation(), items, port.ProjectRef{}, "A Project", "", nil, time.Now())
	if doc.StalledCount != nil {
		t.Errorf("stalled_count = %d, want nil — no items are assigned", *doc.StalledCount)
	}
}

// StalledCount is a pointer to zero (not nil) when items are assigned but none
// are past their due date. Zero and nil are deliberately different signals.
func TestStalledCountIsZeroWhenAssignedButNotPastDue(t *testing.T) {
	future := time.Now().Add(7 * 24 * time.Hour)
	items := []*model.Item{
		{ItemKey: "a", Status: model.StatusInProgress, Assignee: "jdoe", DueDate: &future},
	}
	doc := buildProjection(liveFormation(), items, port.ProjectRef{}, "A Project", "", nil, time.Now())
	if doc.StalledCount == nil {
		t.Fatal("stalled_count = nil, want a pointer to 0 — an assigned item is present")
	}
	if *doc.StalledCount != 0 {
		t.Errorf("stalled_count = %d, want 0 — due date is in the future", *doc.StalledCount)
	}
}

// An assigned item past its due date with a non-terminal status counts as
// stalled. Items with no due date are not counted — a fallback for undeadlined
// assigned work requires an assigned_at column that does not yet exist.
func TestStalledCountIncludesPastDueAssignedItems(t *testing.T) {
	yesterday := time.Now().Add(-24 * time.Hour)
	future := time.Now().Add(7 * 24 * time.Hour)
	items := []*model.Item{
		{ItemKey: "a", Status: model.StatusInProgress, Assignee: "jdoe", DueDate: &yesterday},
		{ItemKey: "b", Status: model.StatusInProgress, Assignee: "asmith", DueDate: &future},
		{ItemKey: "c", Status: model.StatusInProgress, Assignee: "bkim"}, // assigned, no due date
	}
	doc := buildProjection(liveFormation(), items, port.ProjectRef{}, "A Project", "", nil, time.Now())
	if doc.StalledCount == nil {
		t.Fatal("stalled_count = nil, want non-nil — assigned items are present")
	}
	if *doc.StalledCount != 1 {
		t.Errorf("stalled_count = %d, want 1 — only the past-due item is stalled", *doc.StalledCount)
	}
}

// Done and skipped items are never stalled, even when they are past their due
// date. Terminal states mean the work is finished or excused; overdue is only
// meaningful for open work.
//
// StalledCount must be a pointer-to-zero (not nil) here: the item IS assigned,
// so the formation has assigned work — the queue cell must say "0 stalled",
// not be suppressed entirely. Nil would wrongly read as "nothing to chase".
func TestTerminalItemsAreNeverStalled(t *testing.T) {
	yesterday := time.Now().Add(-24 * time.Hour)
	for _, status := range []model.ItemStatus{model.StatusDone, model.StatusSkipped} {
		items := []*model.Item{
			// Past-due terminal item — must not raise the count.
			{ItemKey: "a", Status: status, Assignee: "jdoe", DueDate: &yesterday},
		}
		doc := buildProjection(liveFormation(), items, port.ProjectRef{}, "A Project", "", nil, time.Now())
		if doc.StalledCount == nil {
			t.Errorf("status %q: stalled_count = nil, want &0 — the item is assigned, "+
				"so the cell must be present even though no items are stalled", status)
			continue
		}
		if *doc.StalledCount != 0 {
			t.Errorf("status %q: stalled_count = %d, want 0 — terminal items are never stalled",
				status, *doc.StalledCount)
		}
	}
}

// Republishing an unchanged checklist must produce the same StalledCount —
// the field must be stable, just like BlockedItemTitles and Assignees.
func TestStalledCountIsStableOnRepublish(t *testing.T) {
	yesterday := time.Now().Add(-24 * time.Hour)
	now := time.Now()
	items := []*model.Item{
		{ItemKey: "a", Status: model.StatusInProgress, Assignee: "jdoe", DueDate: &yesterday},
	}
	first := buildProjection(liveFormation(), items, port.ProjectRef{}, "A Project", "", nil, now)
	second := buildProjection(liveFormation(), items, port.ProjectRef{}, "A Project", "", nil, now)

	if first.StalledCount == nil || second.StalledCount == nil {
		t.Fatal("stalled_count = nil on one or both publishes, want non-nil")
	}
	if *first.StalledCount != *second.StalledCount {
		t.Errorf("stalled_count changed between identical publishes: %d vs %d",
			*first.StalledCount, *second.StalledCount)
	}
}

// Items without a due date are not counted as stalled. The definition is
// explicit about this: a fallback for undeadlined assigned work requires an
// assigned_at timestamp that does not yet exist on the model.
func TestAssignedItemsWithNoDueDateAreNotStalled(t *testing.T) {
	items := []*model.Item{
		{ItemKey: "a", Status: model.StatusInProgress, Assignee: "jdoe"},
		{ItemKey: "b", Status: model.StatusBlocked, Assignee: "asmith"},
	}
	doc := buildProjection(liveFormation(), items, port.ProjectRef{}, "A Project", "", nil, time.Now())
	if doc.StalledCount == nil {
		t.Fatal("stalled_count = nil, want a pointer — assigned items are present")
	}
	if *doc.StalledCount != 0 {
		t.Errorf("stalled_count = %d, want 0 — no due dates set, so nothing is stalled by definition",
			*doc.StalledCount)
	}
}
