// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

func liveFormation() *model.Formation {
	return &model.Formation{ProjectUID: "project-1", Lifecycle: model.LifecycleLive}
}

// The six counts are published as fields because the queue sorts on them, and
// the items themselves are not in the document for the browser to count.
func TestProjectionCountsEverySixStatuses(t *testing.T) {
	items := []*model.Item{
		{ItemKey: "a", Status: model.StatusNotStarted},
		{ItemKey: "b", Status: model.StatusInProgress},
		{ItemKey: "c", Status: model.StatusInProgress},
		{ItemKey: "d", Status: model.StatusBlocked, Title: "Blocked one"},
		{ItemKey: "e", Status: model.StatusAwaitingAcceptance},
		{ItemKey: "f", Status: model.StatusDone},
		{ItemKey: "g", Status: model.StatusSkipped},
	}
	doc := buildProjection(liveFormation(), items, port.ProjectRef{}, "A Project", "")

	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"not_started", doc.NotStarted, 1},
		{"in_progress", doc.InProgress, 2},
		{"blocked", doc.Blocked, 1},
		{"awaiting_acceptance", doc.AwaitingAcceptance, 1},
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

	withoutDate := buildProjection(liveFormation(), gatedDone, port.ProjectRef{}, "A Project", "")
	if !withoutDate.GatesCleared {
		t.Error("gates_cleared = false with every gating item done, want true")
	}
	if withoutDate.IsActivating {
		t.Error("is_activating = true with no announcement date, want false — " +
			"full readiness needs the date as well")
	}

	withDate := buildProjection(liveFormation(), gatedDone, port.ProjectRef{}, "A Project", "2026-12-01")
	if !withDate.IsActivating {
		t.Error("is_activating = false with gates cleared and a date set, want true")
	}
}

// A checklist with no gating items must not report itself ready on a vacuous
// truth, on either field.
func TestNoGatingItemsIsNeitherClearedNorActivating(t *testing.T) {
	doc := buildProjection(liveFormation(),
		[]*model.Item{{ItemKey: "a", Status: model.StatusDone}},
		port.ProjectRef{}, "A Project", "2026-12-01")

	if doc.GatesCleared {
		t.Error("gates_cleared = true with zero gating items, want false")
	}
	if doc.IsActivating {
		t.Error("is_activating = true with zero gating items, want false")
	}
}

// awaiting_acceptance is the whole reason the status exists: a claim must not
// satisfy a gate before someone accepts it. skipped must not either — a skipped
// gating item was excused, not completed.
func TestNeitherAClaimNorASkipSatisfiesAGate(t *testing.T) {
	for _, status := range []model.ItemStatus{model.StatusAwaitingAcceptance, model.StatusSkipped} {
		doc := buildProjection(liveFormation(),
			[]*model.Item{{ItemKey: "gate", Gate: true, Status: status}},
			port.ProjectRef{}, "A Project", "2026-12-01")

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
		"A Project", "")

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
	}, port.ProjectRef{}, "A Project", "")

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
	first := buildProjection(liveFormation(), items, port.ProjectRef{}, "A Project", "")

	reordered := []*model.Item{items[1], items[0]}
	second := buildProjection(liveFormation(), reordered, port.ProjectRef{}, "A Project", "")

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
