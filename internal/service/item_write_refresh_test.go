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

	_, err := assignItem(s, asPrincipal("admin"), &svc.AssignItemPayload{
		ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey,
		IfMatch: item.Revision, Assignee: &assignee,
	})
	require.NoError(t, err)

	// Exactly one, and naming the project that was written. More than one would
	// mean a route refreshing per field rather than per write, which multiplies
	// three NATS round trips by however many fields a payload carries.
	require.Equal(t, []string{formation.ProjectUID}, refresher.asked())
}

// Every kind of item write refreshes, not only the ones that move an item
// between queues, and every one of the three routes does it.
//
// The published item document carries the due date, the lifecycle, the title
// and more besides, and the refresh rebuilds the whole projection either way —
// so narrowing this to status and assignee would save nothing and leave a
// changed due date showing its old value for a day.
//
// The table spans all three routes deliberately. They are guarded differently
// and implemented separately, so "a write refreshes" has to be established per
// route rather than inferred from one of them.
func TestEveryItemWriteRouteRefreshesTheIndex(t *testing.T) {
	tests := []struct {
		name  string
		write func(s *Service, formation *model.Formation, item *model.Item) error
	}{
		{"a status change", func(s *Service, f *model.Formation, item *model.Item) error {
			_, err := setStatus(s, asPrincipal("admin"), &svc.SetItemStatusPayload{
				ProjectUID: f.ProjectUID, ItemKey: item.ItemKey, IfMatch: item.Revision,
				Status: ptr(string(model.StatusInProgress)),
			})
			return err
		}},
		{"an assignment", func(s *Service, f *model.Formation, item *model.Item) error {
			_, err := assignItem(s, asPrincipal("admin"), &svc.AssignItemPayload{
				ProjectUID: f.ProjectUID, ItemKey: item.ItemKey, IfMatch: item.Revision,
				Assignee: ptr("jdoe"),
			})
			return err
		}},
		{"a due date alone", func(s *Service, f *model.Formation, item *model.Item) error {
			_, err := assignItem(s, asPrincipal("admin"), &svc.AssignItemPayload{
				ProjectUID: f.ProjectUID, ItemKey: item.ItemKey, IfMatch: item.Revision,
				DueDate: ptr("2026-12-01"),
			})
			return err
		}},
		{"a note alone", func(s *Service, f *model.Formation, item *model.Item) error {
			_, err := updateItem(s, asPrincipal("admin"), &svc.UpdateItemPayload{
				ProjectUID: f.ProjectUID, ItemKey: item.ItemKey, IfMatch: item.Revision,
				Note: ptr("waiting on legal"),
			})
			return err
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, formation, item, _ := newItemMutatorTestService(t)
			refresher := refresherOf(t, s)

			require.NoError(t, tc.write(s, formation, item))
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
				note := "waiting on legal"
				return &svc.UpdateItemPayload{
					ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey,
					IfMatch: item.Revision + 99, Note: &note,
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
				note := "waiting on legal"
				return &svc.UpdateItemPayload{
					ProjectUID: formation.ProjectUID, ItemKey: "no-such-item",
					IfMatch: item.Revision, Note: &note,
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

// Closing an item and sending it back both move it across the boundary the
// Pending Actions list filters on, so these are the writes whose staleness is
// noticed first: work somebody has finished, still listed as outstanding, or
// work returned to them that the index still calls done.
//
// Both went through their own routes before the status model collapsed to five
// values; they are ordinary PATCHes now, and the freshness requirement did not
// move with them.
func TestTerminalWritesRefreshTheIndex(t *testing.T) {
	reason := "the evidence link is dead"

	tests := []struct {
		name    string
		payload func(f *model.Formation, item *model.Item, version int64) *svc.SetItemStatusPayload
	}{
		{
			name: "closing it",
			payload: func(f *model.Formation, item *model.Item, version int64) *svc.SetItemStatusPayload {
				return &svc.SetItemStatusPayload{
					ProjectUID: f.ProjectUID, ItemKey: item.ItemKey,
					IfMatch: version, Status: ptr(string(model.StatusDone)),
				}
			},
		},
		{
			name: "sending it back",
			payload: func(f *model.Formation, item *model.Item, version int64) *svc.SetItemStatusPayload {
				return &svc.SetItemStatusPayload{
					ProjectUID: f.ProjectUID, ItemKey: item.ItemKey,
					IfMatch: version, Status: ptr(string(model.StatusNotStarted)), Reason: &reason,
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, formation, item, _ := newItemMutatorTestService(t)
			refresher := refresherOf(t, s)

			started, err := setStatus(s, asPrincipal("jdoe"), &svc.SetItemStatusPayload{
				ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey,
				IfMatch: item.Revision, Status: ptr(string(model.StatusInProgress)),
			})
			require.NoError(t, err)
			// That first write refreshes too. Cleared so this asserts on the
			// terminal one alone.
			refresher.reset()

			_, err = setStatus(s, asPrincipal("reviewer"), tc.payload(formation, item, started.Version))
			require.NoError(t, err)
			require.Equal(t, []string{"project-1"}, refresher.asked())
		})
	}
}

// A deployment with no NATS wires no refresher, and its write routes must still
// serve. Freshness is the one thing here allowed to be absent — the sweep still
// publishes on its own interval.
func TestAWriteWithNoRefresherWiredStillSucceeds(t *testing.T) {
	s, formation, item, _ := newItemMutatorTestService(t)
	s.refresher = nil
	status := string(model.StatusInProgress)

	updated, err := setStatus(s, asPrincipal("admin"), &svc.SetItemStatusPayload{
		ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey,
		IfMatch: item.Revision, Status: &status,
	})
	require.NoError(t, err)
	require.Equal(t, status, updated.Status)
}

// Skipping is the other way an item reaches a terminal status. Covered
// separately from closing so that "a terminal item leaves the queue promptly"
// holds for both ways of getting there.
func TestSkippingAnItemRefreshesTheIndex(t *testing.T) {
	s, formation, item, _ := newItemMutatorTestService(t)
	refresher := refresherOf(t, s)
	reason := "not applicable to this project"

	_, err := setStatus(s, asPrincipal("admin"), &svc.SetItemStatusPayload{
		ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey, IfMatch: item.Revision,
		Status: ptr(string(model.StatusSkipped)), Reason: &reason,
	})
	require.NoError(t, err)
	require.Equal(t, []string{formation.ProjectUID}, refresher.asked())
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

	// Two routes now, so two writes. Both are asserted against the same
	// published document, because the refresh rebuilds the whole projection
	// rather than patching the field that changed.
	assigned, err := assignItem(s, asPrincipal("admin"), &svc.AssignItemPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey, IfMatch: item.Revision,
		Assignee: &assignee,
	})
	require.NoError(t, err)

	_, err = setStatus(s, asPrincipal("admin"), &svc.SetItemStatusPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey, IfMatch: assigned.Version,
		Status: &status,
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

	result, err := s.SetItemStatus(asPrincipal("admin"), &svc.SetItemStatusPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey, IfMatch: item.Revision,
		Status: ptr(string(model.StatusInProgress)),
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

	result, err := s.SetItemStatus(asPrincipal("admin"), &svc.SetItemStatusPayload{
		ProjectUID: "project-1", ItemKey: item.ItemKey, IfMatch: item.Revision,
		Status: ptr(string(model.StatusInProgress)),
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
