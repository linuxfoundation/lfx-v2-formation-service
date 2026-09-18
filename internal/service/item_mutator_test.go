// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// newItemMutatorTestService wires a Service over fresh in-memory doubles,
// including a unit of work over those same doubles, and seeds a formation
// with two items so tests can exercise them independently.
func newItemMutatorTestService(t *testing.T) (*Service, *model.Formation, *model.Item, *model.Item) {
	t.Helper()

	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	templates := mock.NewTemplateRepository()
	activity := mock.NewActivityRepository()
	uow := mock.NewUnitOfWork(formations, items, activity, templates)

	template, err := templates.Upsert(context.Background(), &model.Template{
		Name: "default", Version: 1, State: model.TemplatePublished,
		Sections: []model.TemplateSection{{Key: "sec-1", Title: "Section One"}},
	})
	require.NoError(t, err)

	formation, err := formations.Create(context.Background(), &model.Formation{
		ProjectUID: "project-1", TemplateUID: template.UID, TemplateVersion: template.Version,
	})
	require.NoError(t, err)

	_, err = items.InsertMany(context.Background(), []*model.Item{
		{FormationUID: formation.UID, ItemKey: "item-1", SectionKey: "sec-1", Title: "Item One", Status: model.StatusNotStarted},
		{FormationUID: formation.UID, ItemKey: "item-2", SectionKey: "sec-1", Title: "Item Two", Status: model.StatusNotStarted},
	})
	require.NoError(t, err)

	itemOne, err := items.GetByKey(context.Background(), formation.UID, "item-1")
	require.NoError(t, err)
	itemTwo, err := items.GetByKey(context.Background(), formation.UID, "item-2")
	require.NoError(t, err)

	s := NewService(
		WithFormations(formations),
		WithItems(items),
		WithTemplates(templates),
		WithActivity(activity),
		WithUnitOfWork(uow),
		// Wired for every test, not only the ones asserting on it. A route that
		// stopped refreshing would otherwise keep passing everywhere the
		// refresher was left out, which is most places.
		WithRefresher(&recordingRefresher{}),
	)
	return s, formation, itemOne, itemTwo
}

// recordingRefresher records which projects a write asked to refresh.
//
// Synchronous, unlike the real one. What these tests are checking is that the
// route asks at all and asks once — the asking is the route's behaviour, and
// what happens afterwards is the refresher's, which has its own tests. Driving
// the real refresher here would make every route test wait on a goroutine to
// assert something that has already happened by the time the route returns.
type recordingRefresher struct {
	mu       sync.Mutex
	projects []string
}

func (r *recordingRefresher) AfterItemWrite(_ context.Context, projectUID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.projects = append(r.projects, projectUID)
}

func (r *recordingRefresher) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.projects...)
}

func (r *recordingRefresher) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.projects = nil
}

// refresherOf reads back the fixture's recorder.
//
// Fails rather than returning nil when the service has no recorder, so a
// fixture that stopped wiring one is reported as the fixture problem it is
// instead of as an assertion about refreshing.
func refresherOf(t *testing.T, s *Service) *recordingRefresher {
	t.Helper()

	recorder, ok := s.refresher.(*recordingRefresher)
	if !ok {
		t.Fatalf("the fixture's refresher is %T, want *recordingRefresher", s.refresher)
	}
	return recorder
}

func TestUpdateItem(t *testing.T) {
	t.Run("two people editing different items both succeed", func(t *testing.T) {
		s, formation, itemOne, itemTwo := newItemMutatorTestService(t)

		res1, err1 := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("in_progress"),
		})
		res2, err2 := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemTwo.ItemKey, IfMatch: itemTwo.Revision,
			Status: ptr("in_progress"),
		})

		require.NoError(t, err1)
		require.NoError(t, err2)
		assert.Equal(t, "in_progress", res1.Status)
		assert.Equal(t, "in_progress", res2.Status)
		assert.Equal(t, itemOne.Revision+1, res1.Version)
		assert.Equal(t, itemTwo.Revision+1, res2.Version)
	})

	t.Run("a null sub_items entry is refused, not a panic", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)

		// Goa's generated validator steps over nil elements without
		// reporting them, so `"sub_items":[null]` reaches the service as a
		// live nil. It used to reach a field access and take the handler
		// down with a nil dereference.
		_, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			SubItems: []*svc.FormationSubItemUpdate{nil},
		})

		require.Error(t, err)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "BadRequest", formationErr.Name)
		assert.Equal(t, "400", formationErr.Code)
		assert.Equal(t, "sub_item_null", formationErr.Reason)

		// The refusal must leave the item untouched rather than half-applied.
		got, getErr := s.items.Get(context.Background(), itemOne.UID)
		require.NoError(t, getErr)
		assert.Equal(t, itemOne.Revision, got.Revision, "the refused mutation must not have bumped the revision")
	})

	t.Run("a skipped item can still be patched on an unrelated field", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		reason := "not applicable to this project"

		first, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("skipped"), Reason: &reason,
		})
		require.NoError(t, err)

		// A skipped item is still editable. Its reason cannot be cleared from
		// here at all now that it travels with the transition, so the field
		// this route does carry must not disturb it.
		note := "still worth recording why"
		got, err := updateItem(s, context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: first.Version,
			Note: &note,
		})

		require.NoError(t, err)
		assert.Equal(t, "skipped", got.Status)
	})

	t.Run("a whitespace-only skip reason is refused", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		blank := "   "

		// Postgres compares btrim(skip_reason), so this passed an == "" test
		// in the service and failed only in the database.
		_, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("skipped"), Reason: &blank,
		})

		require.Error(t, err)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "skip_reason_required", formationErr.Reason)
	})

	t.Run("an unknown sub-item key is refused, not appended", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)

		// Appending would have created a sub-item with no title, and let a
		// status-only route change the checklist's structure.
		_, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			SubItems: []*svc.FormationSubItemUpdate{{Key: "not-a-real-key", Status: "done"}},
		})

		require.Error(t, err)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "BadRequest", formationErr.Name)
		assert.Equal(t, "unknown_sub_item_key", formationErr.Reason)
		assert.Contains(t, formationErr.Message, "not-a-real-key")

		got, getErr := s.items.Get(context.Background(), itemOne.UID)
		require.NoError(t, getErr)
		assert.Equal(t, itemOne.Revision, got.Revision, "the refused mutation must not have bumped the revision")
	})

	t.Run("stale precondition on the same item is refused with nothing lost", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		blockedReason := "waiting on the partner"

		// First writer succeeds and moves the item's revision forward.
		first, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("in_progress"),
		})
		require.NoError(t, err)

		// Second writer still holds the pre-update revision.
		result, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("blocked"), Reason: &blockedReason,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "VersionMismatch", formationErr.Name)
		assert.Equal(t, "412", formationErr.Code)
		assert.Equal(t, "version_mismatch", formationErr.Reason)

		// Nothing lost: the first writer's change is still there.
		got, err := s.items.Get(context.Background(), itemOne.UID)
		require.NoError(t, err)
		assert.Equal(t, model.StatusInProgress, got.Status)
		assert.Equal(t, first.Version, got.Revision)
	})

	t.Run("skip with no reason is refused", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)

		result, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("skipped"),
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "BadRequest", formationErr.Name)
		assert.Equal(t, "400", formationErr.Code)
		assert.Equal(t, "skip_reason_required", formationErr.Reason)

		got, err := s.items.Get(context.Background(), itemOne.UID)
		require.NoError(t, err)
		assert.Equal(t, model.StatusNotStarted, got.Status, "the refused mutation must not have changed the item")
	})

	t.Run("blocking with no reason names the blocked requirement", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)

		result, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("blocked"),
		})

		require.Nil(t, result)
		formationErr := formationError(t, err)
		assert.Equal(t, "blocked_reason_required", formationErr.Reason)
		assert.Equal(t, "a reason is required to block an item", formationErr.Message)
	})

	t.Run("skip with a reason succeeds", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		reason := "blocked by legal review"

		result, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("skipped"), Reason: &reason,
		})

		require.NoError(t, err)
		assert.Equal(t, "skipped", result.Status)
		require.NotNil(t, result.SkipReason)
		assert.Equal(t, reason, *result.SkipReason)
	})

	t.Run("unrecognised item key is refused naming the key", func(t *testing.T) {
		s, formation, _, _ := newItemMutatorTestService(t)
		note := "hello"

		result, err := updateItem(s, context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: "no-such-key", IfMatch: 1, Note: &note,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "NotFound", formationErr.Name)
		assert.Equal(t, "unknown_item_key", formationErr.Reason)
		assert.Contains(t, formationErr.Message, "no-such-key")
	})

	t.Run("no formation for project is refused", func(t *testing.T) {
		s, _, _, _ := newItemMutatorTestService(t)
		note := "hello"

		result, err := updateItem(s, context.Background(), &svc.UpdateItemPayload{
			ProjectUID: "no-such-project", ItemKey: "item-1", IfMatch: 1, Note: &note,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "NotFound", formationErr.Name)
		assert.Equal(t, "not_found", formationErr.Reason)
	})

	t.Run("invalid transition is refused", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)

		// A closed item reopens — to not started or in progress — and that is
		// all. Excusing it outright is not a reversal of closing it, and
		// getting there means reopening it first, on the record.
		closed, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("done"),
		})
		require.NoError(t, err)

		excuse := "turned out not to apply"
		result, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: closed.Version,
			Status: ptr("skipped"), Reason: &excuse,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "Conflict", formationErr.Name)
		assert.Equal(t, "409", formationErr.Code)
		assert.Equal(t, "invalid_transition", formationErr.Reason)
	})

	t.Run("in_progress to done closes the item in one step", func(t *testing.T) {
		// This used to be refused. done was reachable only by claiming
		// completion and having somebody else accept it, and the refusal here
		// was what forced a caller onto that narrower-guarded route.
		//
		// The claim state is gone and the safeguard it existed for did not go
		// with it: closing an item is a status change, every status change
		// enters through this route, and the gateway admits this route only
		// for the formation team. An assignee elevated to writer to do the
		// work is not on that team, so they still cannot close their own item
		// — the refusal moved from the transition table to the guard.
		s, formation, itemOne, _ := newItemMutatorTestService(t)

		started, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("in_progress"),
		})
		require.NoError(t, err)

		result, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: started.Version,
			Status: ptr("done"),
		})

		require.NoError(t, err)
		assert.Equal(t, "done", result.Status)
	})

	t.Run("a closed item goes back with a reason, and needs one", func(t *testing.T) {
		// The reversal of closing. A reviewer who decides the work was not
		// done correctly has to be able to return it, and returning it
		// silently leaves the assignee nothing to act on — so the reason is
		// required rather than merely accepted.
		s, formation, itemOne, _ := newItemMutatorTestService(t)

		closed, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("done"),
		})
		require.NoError(t, err)

		_, err = setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: closed.Version,
			Status: ptr("not_started"),
		})
		require.Error(t, err, "sending an item back with no reason")
		formationErr := formationError(t, err)
		assert.Equal(t, "return_reason_required", formationErr.Reason)
		assert.Equal(t, "a reason is required to send an item back to not started", formationErr.Message)

		reason := "the evidence link is dead"
		result, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: closed.Version,
			Status: ptr("not_started"), Reason: &reason,
		})

		require.NoError(t, err)
		assert.Equal(t, "not_started", result.Status)
		require.NotNil(t, result.Note)
		assert.Equal(t, reason, *result.Note)
	})

	t.Run("checklist read-only refuses every mutation", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		note := "hello"

		_, err := s.formations.UpdateLifecycle(context.Background(), formation.UID, model.LifecycleCompleted, formation.Revision)
		require.NoError(t, err)

		result, err := updateItem(s, context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Note: &note,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "Conflict", formationErr.Name)
		assert.Equal(t, "checklist_read_only", formationErr.Reason)
	})

	// The refusal above is only as good as the read it is made from. An
	// unlocked read can be overtaken by a freeze committing between the check
	// and the item write, which is the one way a mutation lands on a checklist
	// that is no longer live — the item's own revision cannot catch it, since
	// the lifecycle moves on the formation row and carries its own.
	//
	// Asserted here as the call the write path makes; that the lock actually
	// blocks is Postgres's behaviour and is exercised against a real database
	// in the repository tests.
	t.Run("the mutation reads the checklist's lifecycle under a row lock", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		formations, ok := s.formations.(*mock.FormationRepository)
		require.True(t, ok, "the fixture's formation repository is %T", s.formations)
		formations.ResetCalls()

		_, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("in_progress"),
		})
		require.NoError(t, err)

		calls := formations.Calls()
		assert.Equal(t, 1, calls["formations.GetByProjectForUpdate"],
			"the write path did not lock the formation row it checked the lifecycle on")
		assert.Zero(t, calls["formations.GetByProject"],
			"the write path took the unlocked read, which a concurrent freeze can overtake")
	})

	t.Run("evidence link with an unsafe scheme is refused", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		unsafe := "javascript:alert(1)"

		result, err := updateItem(s, context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, EvidenceLink: &unsafe,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "link_scheme_invalid", formationErr.Reason)
	})

	t.Run("assignee with no standing on the project is refused", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		projects := mock.NewProjectReader()
		projects.SetSettings(formation.ProjectUID, &port.ProjectSettings{
			ProjectUID: formation.ProjectUID, Writers: []string{"alice"},
		})
		s.projects = projects

		outsider := "mallory"
		result, err := assignItem(s, context.Background(), &svc.AssignItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Assignee: &outsider,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "assignee_not_on_project", formationErr.Reason)
	})

	// The counterpart to the case above, and the distinction the whole check
	// rests on: a roster that was read and does not list the assignee refuses,
	// a roster that could not be read does not.
	//
	// The project service answers every handler failure with an empty reply, so a
	// KV error and a genuinely absent settings record are indistinguishable on
	// the wire. Refusing here would answer a transient upstream fault by telling
	// someone their colleague is not on the project.
	t.Run("an unreadable roster accepts the assignee rather than refusing", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		// No SetSettings, so the double reports the project as having no roster —
		// exactly what an empty reply from the project service decodes to.
		s.projects = mock.NewProjectReader()

		assignee := "someone-unverifiable"
		result, err := assignItem(s, context.Background(), &svc.AssignItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey,
			IfMatch: itemOne.Revision, Assignee: &assignee,
		})

		require.NoError(t, err, "an unreadable roster must not fail the update")
		require.NotNil(t, result)
		require.NotNil(t, result.Assignee)
		assert.Equal(t, assignee, *result.Assignee)
	})

	// The assignee's membership is checked before the transaction opens, since
	// it is a call to another service — but it must still be reported after the
	// precondition, or a stale write would answer 400 where it answers 412 and
	// the client's retry logic would stop retrying.
	t.Run("a stale precondition outranks a bad assignee", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		projects := mock.NewProjectReader()
		projects.SetSettings(formation.ProjectUID, &port.ProjectSettings{
			ProjectUID: formation.ProjectUID, Writers: []string{"alice"},
		})
		s.projects = projects

		outsider := "mallory"
		result, err := assignItem(s, context.Background(), &svc.AssignItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey,
			IfMatch:  itemOne.Revision + 99,
			Assignee: &outsider,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "version_mismatch", formationErr.Reason,
			"the precondition is checked under the lock and must be reported first")
	})

	// Same ordering question for the checklist that cannot be written at all.
	t.Run("a read-only checklist outranks a bad assignee", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		projects := mock.NewProjectReader()
		projects.SetSettings(formation.ProjectUID, &port.ProjectSettings{
			ProjectUID: formation.ProjectUID, Writers: []string{"alice"},
		})
		s.projects = projects
		_, freezeErr := s.formations.UpdateLifecycle(
			context.Background(), formation.UID, model.LifecycleFrozen, formation.Revision,
		)
		require.NoError(t, freezeErr)

		outsider := "mallory"
		result, err := assignItem(s, context.Background(), &svc.AssignItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey,
			IfMatch:  itemOne.Revision,
			Assignee: &outsider,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "checklist_read_only", formationErr.Reason)
	})

	// The assignee is checked against another service, so that check runs
	// before the transaction opens rather than holding a row lock across a
	// network call. Moving it there must not change which refusal a caller
	// sees: a request that is wrong about both the precondition and the
	// assignee is still answered on the precondition. The first attempt at
	// moving it got this wrong — it hoisted the refusal to the front and
	// turned this 412 into a 400.
	t.Run("a stale precondition outranks a bad assignee", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		projects := mock.NewProjectReader()
		projects.SetSettings(formation.ProjectUID, &port.ProjectSettings{
			ProjectUID: formation.ProjectUID, Writers: []string{"alice"},
		})
		s.projects = projects

		outsider := "mallory"
		result, err := assignItem(s, context.Background(), &svc.AssignItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision + 99,
			Assignee: &outsider,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "version_mismatch", formationErr.Reason)
		assert.Equal(t, "VersionMismatch", formationErr.Name)
	})

	t.Run("assignee holding a project grant succeeds", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		projects := mock.NewProjectReader()
		projects.SetSettings(formation.ProjectUID, &port.ProjectSettings{
			ProjectUID: formation.ProjectUID, Writers: []string{"alice"},
		})
		s.projects = projects

		alice := "alice"
		result, err := assignItem(s, context.Background(), &svc.AssignItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Assignee: &alice,
		})

		require.NoError(t, err)
		require.NotNil(t, result.Assignee)
		assert.Equal(t, "alice", *result.Assignee)
	})

	t.Run("no unit of work wired fails closed", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		s.uow = nil
		note := "hello"

		result, err := updateItem(s, context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Note: &note,
		})

		require.Nil(t, result)
		require.Error(t, err)
		var formationErr *svc.FormationError
		assert.False(t, errors.As(err, &formationErr), "an unwired dependency is a 500, not a declared error")
	})

	t.Run("patching one sub-item leaves the others untouched", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)

		// itemOne was seeded with no sub-items; give it two directly through
		// the repository double, bypassing UpdateItem, so the PATCH below
		// is the only mutation under test.
		withSubItems, err := s.items.Update(context.Background(), itemOne.UID, itemOne.Revision, port.ItemPatch{
			SubItems: &[]model.SubItem{
				{Key: "sub-a", Title: "Sub A", Status: model.StatusNotStarted},
				{Key: "sub-b", Title: "Sub B", Status: model.StatusNotStarted},
			},
		})
		require.NoError(t, err)

		result, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: withSubItems.Revision,
			SubItems: []*svc.FormationSubItemUpdate{{Key: "sub-a", Status: "done"}},
		})

		require.NoError(t, err)
		require.Len(t, result.SubItems, 2, "sub-b must survive a PATCH that only names sub-a")
		byKey := map[string]*svc.FormationSubItem{}
		for _, si := range result.SubItems {
			byKey[si.Key] = si
		}
		require.Contains(t, byKey, "sub-a")
		require.Contains(t, byKey, "sub-b")
		assert.Equal(t, "done", byKey["sub-a"].Status)
		assert.Equal(t, "not_started", byKey["sub-b"].Status, "unmentioned sub-item's status must be unchanged")
		assert.Equal(t, "Sub B", byKey["sub-b"].Title, "unmentioned sub-item's title must be carried over")
	})

	t.Run("due_date can be set then cleared with an empty string", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		due := "2026-08-31"

		set, err := assignItem(s, context.Background(), &svc.AssignItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, DueDate: &due,
		})
		require.NoError(t, err)
		require.NotNil(t, set.DueDate)
		assert.Equal(t, due, *set.DueDate)

		empty := ""
		cleared, err := assignItem(s, context.Background(), &svc.AssignItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision + 1, DueDate: &empty,
		})
		require.NoError(t, err)
		assert.Nil(t, cleared.DueDate, "an empty string must clear due_date")
	})

	t.Run("malformed due_date is refused", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		bad := "not-a-date"

		result, err := assignItem(s, context.Background(), &svc.AssignItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, DueDate: &bad,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "BadRequest", formationErr.Name)
		assert.Equal(t, "due_date_invalid", formationErr.Reason)
	})

	t.Run("re-skipping an already-skipped item is refused as a no-op", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		reason := "blocked by legal review"

		afterSkip, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: ptr("skipped"), Reason: &reason,
		})
		require.NoError(t, err)

		// Sending the status it already holds is refused before the reason is
		// even looked at. The stored reason is therefore unreachable from
		// here, which is what the old empty-skip_reason hazard needed.
		emptyReason := ""
		result, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: afterSkip.Version,
			Status: ptr("skipped"), Reason: &emptyReason,
		})

		require.Nil(t, result)
		assert.Equal(t, "no_fields_to_update", formationError(t, err).Reason)

		got, getErr := s.items.Get(context.Background(), itemOne.UID)
		require.NoError(t, getErr)
		assert.Equal(t, reason, got.SkipReason, "the stored reason must survive the refusal")
	})

	t.Run("an empty sub-items update is refused as a no-op", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)

		result, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			SubItems: []*svc.FormationSubItemUpdate{},
		})

		require.Nil(t, result)
		assert.Equal(t, "no_fields_to_update", formationError(t, err).Reason)

		got, getErr := s.items.Get(context.Background(), itemOne.UID)
		require.NoError(t, getErr)
		assert.Equal(t, itemOne.Revision, got.Revision, "a no-op must not consume the ETag")
	})

	t.Run("unchanged parent and sub-item statuses are refused as a no-op", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)

		withSubItem, err := s.items.Update(context.Background(), itemOne.UID, itemOne.Revision, port.ItemPatch{
			SubItems: &[]model.SubItem{
				{Key: "sub-a", Title: "Sub A", Status: model.StatusNotStarted},
			},
		})
		require.NoError(t, err)

		result, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: withSubItem.Revision,
			Status:   ptr("not_started"),
			SubItems: []*svc.FormationSubItemUpdate{{Key: "sub-a", Status: "not_started"}},
		})

		require.Nil(t, result)
		assert.Equal(t, "no_fields_to_update", formationError(t, err).Reason)

		got, getErr := s.items.Get(context.Background(), itemOne.UID)
		require.NoError(t, getErr)
		assert.Equal(t, withSubItem.Revision, got.Revision, "a no-op must not consume the ETag")
	})
}

// The activity feed names an entry after the field the caller led with, so the
// precedence between cases is the behaviour worth pinning, not just the
// mapping. Status, assignment and sub-items are absent because they are not
// this route's to carry: each enters through a route guarded differently, and
// each names its own entry there.
func TestMutationAction(t *testing.T) {
	str := func(s string) *string { return &s }

	tests := []struct {
		name string
		p    *svc.UpdateItemPayload
		want string
	}{
		{"evidence link alone", &svc.UpdateItemPayload{EvidenceLink: str("https://example.test")}, "evidence_link_changed"},
		{"note alone", &svc.UpdateItemPayload{Note: str("a note")}, "note_changed"},
		{
			"the evidence link wins over a co-submitted note",
			&svc.UpdateItemPayload{EvidenceLink: str("https://example.test"), Note: str("a note")},
			"evidence_link_changed",
		},
		{"nothing recognised", &svc.UpdateItemPayload{}, "item_updated"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mutationAction(tc.p))
		})
	}
}

// The ETag exists to be sent straight back as the next If-Match. A value that
// does not parse as the Int64 that header expects would be worse than no
// header at all, so the round trip is asserted rather than the format alone.
func TestSetItemStatusReturnsAnETagThatWorksAsTheNextIfMatch(t *testing.T) {
	s, formation, itemOne, _ := newItemMutatorTestService(t)

	first, err := s.SetItemStatus(context.Background(), &svc.SetItemStatusPayload{
		ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
		Status: ptr("in_progress"),
	})
	require.NoError(t, err)
	require.NotNil(t, first.Etag)
	assert.Equal(t, strconv.FormatInt(first.Item.Version, 10), *first.Etag)

	ifMatch, err := strconv.ParseInt(*first.Etag, 10, 64)
	require.NoError(t, err, "the ETag we hand out must parse as the If-Match we demand")

	blockedReason := "waiting on the partner"
	second, err := setStatus(s, context.Background(), &svc.SetItemStatusPayload{
		ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: ifMatch,
		Status: ptr("blocked"), Reason: &blockedReason,
	})
	require.NoError(t, err)
	assert.Equal(t, first.Item.Version+1, second.Version)
}

func TestUpdateItemRefusesANoOp(t *testing.T) {
	s, formation, itemOne, _ := newItemMutatorTestService(t)

	// Update always increments the revision, so applying this would have
	// invalidated other clients' if_match and logged a change that never
	// happened.
	_, err := updateItem(s, context.Background(), &svc.UpdateItemPayload{
		ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
	})

	require.Error(t, err)
	var formationErr *svc.FormationError
	require.ErrorAs(t, err, &formationErr)
	assert.Equal(t, "BadRequest", formationErr.Name)
	assert.Equal(t, "no_fields_to_update", formationErr.Reason)

	got, getErr := s.items.Get(context.Background(), itemOne.UID)
	require.NoError(t, getErr)
	assert.Equal(t, itemOne.Revision, got.Revision, "a refused no-op must not bump the revision")
}

func TestUpdateItemRefusesYearZeroDueDate(t *testing.T) {
	s, formation, itemOne, _ := newItemMutatorTestService(t)
	yearZero := "0000-01-01"

	// Go's parser accepts this and Postgres has no year zero, so it used to
	// pass validation and fail in the DATE column as a 500.
	_, err := assignItem(s, context.Background(), &svc.AssignItemPayload{
		ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
		DueDate: &yearZero,
	})

	require.Error(t, err)
	var formationErr *svc.FormationError
	require.ErrorAs(t, err, &formationErr)
	assert.Equal(t, "BadRequest", formationErr.Name)
	assert.Equal(t, "due_date_invalid", formationErr.Reason)
}

// --- item-assigned email dispatch tests ---

// newEmailTestService extends newItemMutatorTestService with a wired mock
// emailer and a project reader that seeds username→email so dispatch can
// resolve a real address. Pass empty email to simulate a user with no
// email on record (bare-username case).
func newEmailTestService(t *testing.T, username, email string) (*Service, *model.Formation, *model.Item, *mock.EmailDispatcher) {
	t.Helper()
	s, formation, itemOne, _ := newItemMutatorTestService(t)

	mailer := mock.NewEmailDispatcher()
	s.emailer = mailer
	s.emailCfg = EmailConfig{Enabled: true, AdminBaseURL: "https://app.lfx.dev"}

	settings := &port.ProjectSettings{
		ProjectUID: formation.ProjectUID,
		Writers:    []string{username},
		UserEmails: map[string]string{},
	}
	if email != "" {
		settings.UserEmails[username] = email
	}
	projects := mock.NewProjectReader()
	projects.SetSettings(formation.ProjectUID, settings)
	projects.SetFormingProjects([]port.ProjectRef{
		{UID: formation.ProjectUID, Slug: "test-project", ParentUID: "parent-foundation-uid"},
	})
	projects.SetSlug("parent-foundation-uid", "parent-foundation")
	s.projects = projects

	return s, formation, itemOne, mailer
}

func TestItemAssignedEmailDispatchedOnAssigneeSet(t *testing.T) {
	// username is what gets stored in item.Assignee; email is what the project
	// service carries alongside it and what the notification must be sent to.
	username := "alice"
	addr := "alice@example.com"
	s, formation, itemOne, mailer := newEmailTestService(t, username, addr)

	_, err := s.AssignItem(context.Background(), &svc.AssignItemPayload{
		ProjectUID: formation.ProjectUID,
		ItemKey:    itemOne.ItemKey,
		IfMatch:    itemOne.Revision,
		Assignee:   &username,
	})

	require.NoError(t, err)
	assert.Equal(t, 1, mailer.SentCount(), "expected one item-assigned email")
	sent := mailer.Sent()[0]
	assert.Equal(t, addr, sent.To, "email must be sent to the resolved address, not the bare username")
	wantURL := "https://app.lfx.dev/foundation/formations/test-project?project=parent-foundation"
	assert.Contains(t, sent.Text, wantURL, "email text must contain the LFX One deep link")
}

func TestItemAssignedEmailNotDispatchedWhenAssigneeUnchanged(t *testing.T) {
	// Guards the prevAssignee fix: a PATCH that repeats the current assignee
	// while changing another field (or sending the same state) must not
	// re-send the notification.
	username := "alice"
	addr := "alice@example.com"
	s, formation, itemOne, mailer := newEmailTestService(t, username, addr)

	// First PATCH: sets the assignee — should send one email.
	res, err := s.AssignItem(context.Background(), &svc.AssignItemPayload{
		ProjectUID: formation.ProjectUID,
		ItemKey:    itemOne.ItemKey,
		IfMatch:    itemOne.Revision,
		Assignee:   &username,
	})
	require.NoError(t, err)
	require.Equal(t, 1, mailer.SentCount(), "expected one email after first assignment")

	// Second PATCH: repeats the same assignee — must not send another email.
	_, err = s.AssignItem(context.Background(), &svc.AssignItemPayload{
		ProjectUID: formation.ProjectUID,
		ItemKey:    itemOne.ItemKey,
		IfMatch:    res.Item.Version,
		Assignee:   &username,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, mailer.SentCount(), "no additional email when assignee is unchanged")
}

func TestItemAssignedEmailNotDispatchedWhenAssigneeIsUsername(t *testing.T) {
	// A username with no email entry in UserEmails must not produce a send:
	// there is no address to route to, and the bare username is not a mailbox.
	username := "alice"
	s, formation, itemOne, mailer := newEmailTestService(t, username, "") // no email mapped

	_, err := s.AssignItem(context.Background(), &svc.AssignItemPayload{
		ProjectUID: formation.ProjectUID,
		ItemKey:    itemOne.ItemKey,
		IfMatch:    itemOne.Revision,
		Assignee:   &username,
	})

	require.NoError(t, err)
	assert.Equal(t, 0, mailer.SentCount(), "no email expected when no address is on record")
}

func TestItemAssignedEmailNotDispatchedWhenAssigneeClearedOrEmpty(t *testing.T) {
	s, formation, itemOne, mailer := newEmailTestService(t, "alice", "alice@example.com")

	empty := ""
	_, err := s.AssignItem(context.Background(), &svc.AssignItemPayload{
		ProjectUID: formation.ProjectUID,
		ItemKey:    itemOne.ItemKey,
		IfMatch:    itemOne.Revision,
		Assignee:   &empty,
	})

	require.NoError(t, err)
	assert.Equal(t, 0, mailer.SentCount(), "clearing the assignee must not send an email")
}

func TestItemAssignedEmailNotDispatchedWhenEmailerNil(t *testing.T) {
	s, formation, itemOne, _ := newEmailTestService(t, "alice", "alice@example.com")
	s.emailer = nil // not wired

	username := "alice"
	_, err := s.AssignItem(context.Background(), &svc.AssignItemPayload{
		ProjectUID: formation.ProjectUID,
		ItemKey:    itemOne.ItemKey,
		IfMatch:    itemOne.Revision,
		Assignee:   &username,
	})

	require.NoError(t, err, "nil emailer must not block the write")
}

func TestItemAssignedEmailNotDispatchedWhenEmailDisabled(t *testing.T) {
	s, formation, itemOne, mailer := newEmailTestService(t, "alice", "alice@example.com")
	s.emailCfg.Enabled = false

	username := "alice"
	_, err := s.AssignItem(context.Background(), &svc.AssignItemPayload{
		ProjectUID: formation.ProjectUID,
		ItemKey:    itemOne.ItemKey,
		IfMatch:    itemOne.Revision,
		Assignee:   &username,
	})

	require.NoError(t, err)
	assert.Equal(t, 0, mailer.SentCount(), "disabled email config must suppress dispatch")
}
