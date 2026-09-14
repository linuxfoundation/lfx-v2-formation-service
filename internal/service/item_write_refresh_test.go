// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// An assignment is the write this exists for. Until it triggered a refresh,
// somebody assigned a piece of work saw nothing on their list until the next
// sweep, which runs daily — so the list was wrong for most of the time anybody
// would have looked at it.
func TestUpdateItemRefreshesTheIndex(t *testing.T) {
	s, formation, item, _ := newItemMutatorTestService(t)
	refresher := refresherOf(t, s)
	assignee := "jdoe"

	_, err := updateItem(s, asPrincipal("admin"), &svc.UpdateItemPayload{
		ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey,
		IfMatch: item.Revision, Assignee: &assignee,
	})
	require.NoError(t, err)

	// Exactly one, and naming the project that was written. More than one would
	// mean a route refreshing per field rather than per write, which multiplies
	// three NATS round trips by however many fields a payload carries.
	require.Equal(t, []string{formation.ProjectUID}, refresher.asked())
}

// Every kind of item write refreshes, not only the two that move an item
// between queues.
//
// The published item document carries the due date, the lifecycle, the title
// and more besides, and the refresh rebuilds the whole projection either way —
// so narrowing this to status and assignee would save nothing and leave a
// changed due date showing its old value for a day.
func TestUpdateItemRefreshesForEveryKindOfChange(t *testing.T) {
	dueDate := "2026-12-01"
	note := "waiting on legal"
	status := string(model.StatusInProgress)
	assignee := "jdoe"

	tests := []struct {
		name  string
		apply func(p *svc.UpdateItemPayload)
	}{
		{"the status", func(p *svc.UpdateItemPayload) { p.Status = &status }},
		{"the assignee", func(p *svc.UpdateItemPayload) { p.Assignee = &assignee }},
		{"the due date alone", func(p *svc.UpdateItemPayload) { p.DueDate = &dueDate }},
		{"the note alone", func(p *svc.UpdateItemPayload) { p.Note = &note }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, formation, item, _ := newItemMutatorTestService(t)
			refresher := refresherOf(t, s)

			payload := &svc.UpdateItemPayload{
				ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey, IfMatch: item.Revision,
			}
			tc.apply(payload)

			_, err := updateItem(s, asPrincipal("admin"), payload)
			require.NoError(t, err)
			require.Equal(t, []string{formation.ProjectUID}, refresher.asked())
		})
	}
}

// A write that did not happen must not refresh. Publishing after a rejected
// write would republish the state already in the index, which is harmless but
// buys a stale-index round trip for every failed request — and a request that
// failed on a version conflict is exactly the one somebody is about to retry.
func TestAFailedWriteDoesNotRefresh(t *testing.T) {
	tests := []struct {
		name    string
		payload func(formation *model.Formation, item *model.Item) *svc.UpdateItemPayload
	}{
		{
			name: "the revision is stale",
			payload: func(formation *model.Formation, item *model.Item) *svc.UpdateItemPayload {
				status := string(model.StatusInProgress)
				return &svc.UpdateItemPayload{
					ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey,
					IfMatch: item.Revision + 99, Status: &status,
				}
			},
		},
		{
			// Refused before the write, because Update always increments the
			// revision — so this would invalidate every other client's
			// if_match. Nothing committed means nothing to republish.
			name: "the body changes no field",
			payload: func(formation *model.Formation, item *model.Item) *svc.UpdateItemPayload {
				return &svc.UpdateItemPayload{
					ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey, IfMatch: item.Revision,
				}
			},
		},
		{
			name: "the item does not exist",
			payload: func(formation *model.Formation, item *model.Item) *svc.UpdateItemPayload {
				status := string(model.StatusInProgress)
				return &svc.UpdateItemPayload{
					ProjectUID: formation.ProjectUID, ItemKey: "no-such-item",
					IfMatch: item.Revision, Status: &status,
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, formation, item, _ := newItemMutatorTestService(t)
			refresher := refresherOf(t, s)

			_, err := updateItem(s, asPrincipal("admin"), tc.payload(formation, item))
			require.Error(t, err)
			require.Empty(t, refresher.asked(), "a rejected write asked for a refresh")
		})
	}
}

// Accept, reject and reopen all move an item across the boundary the Pending
// Actions list filters on, so these are the writes whose staleness is noticed
// first: work somebody has finished, still listed as outstanding.
func TestAcceptanceRoutesRefreshTheIndex(t *testing.T) {
	tests := []struct {
		name string
		call func(s *Service, item *model.Item, version int64) error
	}{
		{
			name: "accept",
			call: func(s *Service, item *model.Item, version int64) error {
				_, err := acceptItem(s, asPrincipal("reviewer"), &svc.AcceptItemPayload{
					ProjectUID: "project-1", ItemKey: item.ItemKey, IfMatch: version,
				})
				return err
			},
		},
		{
			name: "reject",
			call: func(s *Service, item *model.Item, version int64) error {
				_, err := rejectItem(s, asPrincipal("reviewer"), &svc.RejectItemPayload{
					ProjectUID: "project-1", ItemKey: item.ItemKey, IfMatch: version,
					Note: "the evidence link is dead",
				})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _, item, _ := newItemMutatorTestService(t)
			refresher := refresherOf(t, s)

			claimed := claim(t, s, item, "jdoe")
			// The claim itself is three ordinary PATCHes, each of which refreshes.
			// Cleared so this asserts on the acceptance write alone.
			refresher.reset()

			require.NoError(t, tc.call(s, item, claimed.Version))
			require.Equal(t, []string{"project-1"}, refresher.asked())
		})
	}
}

// Reopen, the reversal of an acceptance. Separate from the table above because
// it needs an item that has already been accepted, and it is the write whose
// staleness is least forgivable: work has gone back on somebody's list and the
// index would keep saying it was finished.
func TestReopenRefreshesTheIndex(t *testing.T) {
	s, _, item, _ := newItemMutatorTestService(t)
	refresher := refresherOf(t, s)

	claimed := claim(t, s, item, "jdoe")
	accepted, err := acceptItem(s, asPrincipal("reviewer"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey, IfMatch: claimed.Version,
	})
	require.NoError(t, err)
	refresher.reset()

	_, err = reopenItem(s, asPrincipal("reviewer"), &svc.ReopenItemPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey, IfMatch: accepted.Version,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"project-1"}, refresher.asked())
}

// A deployment with no NATS wires no refresher, and its write routes must still
// serve. Freshness is the one thing here allowed to be absent — the sweep still
// publishes on its own interval.
func TestAWriteWithNoRefresherWiredStillSucceeds(t *testing.T) {
	s, formation, item, _ := newItemMutatorTestService(t)
	s.refresher = nil
	status := string(model.StatusInProgress)

	updated, err := updateItem(s, asPrincipal("admin"), &svc.UpdateItemPayload{
		ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey,
		IfMatch: item.Revision, Status: &status,
	})
	require.NoError(t, err)
	require.Equal(t, status, updated.Status)
}

// Skipping an item is the other route to a terminal status, reached through the
// ordinary PATCH rather than through acceptance. Covered so that "a terminal
// item leaves the queue promptly" holds for both ways of getting there.
func TestSkippingAnItemRefreshesTheIndex(t *testing.T) {
	s, formation, item, _ := newItemMutatorTestService(t)
	refresher := refresherOf(t, s)
	skipped, reason := string(model.StatusSkipped), "not applicable to this project"

	_, err := updateItem(s, asPrincipal("admin"), &svc.UpdateItemPayload{
		ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey, IfMatch: item.Revision,
		Status: &skipped, SkipReason: &reason,
	})
	require.NoError(t, err)
	require.Equal(t, []string{formation.ProjectUID}, refresher.asked())
}

// A refused acceptance publishes nothing. The self-acceptance guard runs before
// the write, so there is no committed change to republish — and republishing
// anyway would spend a round trip confirming that nothing happened.
func TestARefusedAcceptanceDoesNotRefresh(t *testing.T) {
	s, _, item, _ := newItemMutatorTestService(t)
	refresher := refresherOf(t, s)

	claimed := claim(t, s, item, "jdoe")
	refresher.reset()

	_, err := acceptItem(s, asPrincipal("jdoe"), &svc.AcceptItemPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey, IfMatch: claimed.Version,
	})
	require.Error(t, err, "the assignee accepted their own item")
	require.Empty(t, refresher.asked())
}

// newLiveRefreshService wires the real refresher behind the write routes, which
// the fixture above deliberately does not.
//
// Two of the claims this feature makes can only be checked end to end: that a
// write still returns its ordinary result when refreshing fails, and that the
// document a write publishes carries the value that was written. A recorder
// proves the route asked; only the real refresher proves what the asking did.
func newLiveRefreshService(t *testing.T) (
	*Service, *model.Item, *mock.IndexerPublisher, *mock.ProjectReader, *Refresher,
) {
	t.Helper()
	ctx := context.Background()

	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	activity := mock.NewActivityRepository()
	templates := mock.NewTemplateRepository()
	uow := mock.NewUnitOfWork(formations, items, activity, templates)
	publisher := mock.NewIndexerPublisher()

	projects := mock.NewProjectReader()
	projects.SetName("project-1", "A Project")
	projects.SetProjectsByUID([]port.ProjectRef{{
		UID: "project-1", Slug: "a-project", SubStage: model.StageFormationEngaged,
	}})

	formation, err := formations.Create(ctx, &model.Formation{ProjectUID: "project-1"})
	require.NoError(t, err)
	_, err = items.InsertMany(ctx, []*model.Item{
		{FormationUID: formation.UID, ItemKey: "item-1", SectionKey: "sec-1",
			Title: "Item One", Status: model.StatusNotStarted},
	})
	require.NoError(t, err)
	item, err := items.GetByKey(ctx, formation.UID, "item-1")
	require.NoError(t, err)

	refresher := NewRefresher(projects, NewProjector(formations, items, projects, publisher))
	s := NewService(
		WithFormations(formations), WithItems(items), WithTemplates(templates),
		WithActivity(activity), WithUnitOfWork(uow), WithProjects(projects),
		WithRefresher(refresher),
	)
	return s, item, publisher, projects, refresher
}

// The document a write publishes carries what was written.
//
// Asserted against values built here rather than against a sweep's output for
// the same project: both paths call the same method, so comparing them passes
// even when the refresh published nothing at all — which is the failure this is
// supposed to catch.
func TestAWriteTriggeredRefreshPublishesTheNewValues(t *testing.T) {
	s, item, publisher, _, refresher := newLiveRefreshService(t)
	assignee := "jdoe"
	status := string(model.StatusInProgress)

	_, err := updateItem(s, asPrincipal("admin"), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey, IfMatch: item.Revision,
		Assignee: &assignee, Status: &status,
	})
	require.NoError(t, err)
	refresher.Stop(context.Background())

	doc := publisher.LatestItem(item.UID.String())
	require.NotNil(t, doc, "no item document was published for the item that was written")
	require.Equal(t, assignee, doc.Assignee)
	require.Equal(t, status, doc.Status)

	// The checklist document is republished from the same read, so its
	// aggregates have to reflect the same change rather than the state before
	// it. This is why the refresh republishes the whole project.
	checklist := publisher.Latest("project-1")
	require.NotNil(t, checklist)
	require.Contains(t, checklist.Assignees, assignee)
}

// A write whose refresh cannot resolve the project still succeeds, still
// returns its ETag, and publishes nothing. The item is committed; the index
// being unreachable is not the writer's problem and the next sweep repairs it.
func TestAWriteSucceedsWhenItsRefreshCannotResolveTheProject(t *testing.T) {
	s, item, publisher, projects, refresher := newLiveRefreshService(t)
	projects.SetRefError(errors.New("no responders available"))
	status := string(model.StatusInProgress)

	result, err := s.UpdateItem(asPrincipal("admin"), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey, IfMatch: item.Revision,
		Status: &status,
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Etag, "a successful write returned no ETag")
	refresher.Stop(context.Background())

	require.Zero(t, publisher.Count())
	require.Equal(t, int64(1), refresher.Counts().Failed, "the failure was not counted")
}

// The same, with the index itself refusing the publish rather than the project
// lookup failing. Both are counted as failures and neither reaches the caller.
func TestAWriteSucceedsWhenItsRefreshCannotPublish(t *testing.T) {
	s, item, publisher, _, refresher := newLiveRefreshService(t)
	publisher.SetError(errors.New("the index is unreachable"))
	status := string(model.StatusInProgress)

	result, err := s.UpdateItem(asPrincipal("admin"), &svc.UpdateItemPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey, IfMatch: item.Revision,
		Status: &status,
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Etag)
	refresher.Stop(context.Background())

	require.Equal(t, int64(1), refresher.Counts().Failed)
}

// Bulk item creation is the sweep's expansion, and it publishes once for the
// whole checklist rather than once per item. Asserted here so the write-path
// refresh cannot later be attached to expansion by analogy with the mutation
// routes: creating a fifty-item checklist would then publish fifty times for
// one logical event.
func TestCreatingAChecklistPublishesOncePerChecklistNotPerItem(t *testing.T) {
	ctx := context.Background()
	ref := port.ProjectRef{UID: "project-1", Slug: "one", SubStage: model.StageFormationEngaged}
	reconciler, fixture, publisher := newReconcilerWithIndex(t, &listProjects{refs: []port.ProjectRef{ref}})

	_, err := reconciler.ReconcileOnce(ctx)
	require.NoError(t, err)

	formation, err := fixture.formations.GetByProject(ctx, ref.UID)
	require.NoError(t, err)
	items, err := fixture.items.ListByFormation(ctx, formation.UID)
	require.NoError(t, err)
	require.Greater(t, len(items), 1, "this test needs a multi-item checklist to mean anything")
	require.Equal(t, 1, publisher.Count(), "expansion published more than one checklist document")
}
