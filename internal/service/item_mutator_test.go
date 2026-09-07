// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
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

	require.NoError(t, items.InsertMany(context.Background(), []*model.Item{
		{FormationUID: formation.UID, ItemKey: "item-1", SectionKey: "sec-1", Title: "Item One", Status: model.StatusNotStarted},
		{FormationUID: formation.UID, ItemKey: "item-2", SectionKey: "sec-1", Title: "Item Two", Status: model.StatusNotStarted},
	}))

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
	)
	return s, formation, itemOne, itemTwo
}

func TestUpdateItem(t *testing.T) {
	t.Run("two people editing different items both succeed", func(t *testing.T) {
		s, formation, itemOne, itemTwo := newItemMutatorTestService(t)
		inProgress := "in_progress"

		res1, err1 := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Status: &inProgress,
		})
		res2, err2 := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemTwo.ItemKey, IfMatch: itemTwo.Revision, Status: &inProgress,
		})

		require.NoError(t, err1)
		require.NoError(t, err2)
		assert.Equal(t, "in_progress", res1.Status)
		assert.Equal(t, "in_progress", res2.Status)
		assert.Equal(t, itemOne.Revision+1, res1.Version)
		assert.Equal(t, itemTwo.Revision+1, res2.Version)
	})

	t.Run("stale precondition on the same item is refused with nothing lost", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		inProgress := "in_progress"
		blocked := "blocked"

		// First writer succeeds and moves the item's revision forward.
		first, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Status: &inProgress,
		})
		require.NoError(t, err)

		// Second writer still holds the pre-update revision.
		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Status: &blocked,
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
		skipped := "skipped"

		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Status: &skipped,
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

	t.Run("skip with a reason succeeds", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		skipped := "skipped"
		reason := "blocked by legal review"

		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: &skipped, SkipReason: &reason,
		})

		require.NoError(t, err)
		assert.Equal(t, "skipped", result.Status)
		require.NotNil(t, result.SkipReason)
		assert.Equal(t, reason, *result.SkipReason)
	})

	t.Run("unrecognised item key is refused naming the key", func(t *testing.T) {
		s, formation, _, _ := newItemMutatorTestService(t)
		note := "hello"

		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
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

		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
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
		done := "done"

		// not_started -> done skips the whole claim/accept dance, which is
		// reserved for the accept route.
		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Status: &done,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "Conflict", formationErr.Name)
		assert.Equal(t, "409", formationErr.Code)
		assert.Equal(t, "invalid_transition", formationErr.Reason)
	})

	t.Run("in_progress to done directly is refused, even for a non-assignee writer", func(t *testing.T) {
		// done is reachable only through awaiting_acceptance + the accept
		// route. A direct in_progress -> done via this Manage-guarded PATCH
		// would let any writer — not just the formation team, and not
		// excluding the item's own assignee — skip acceptance outright,
		// which defeats the reason awaiting_acceptance exists.
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		inProgress := "in_progress"
		done := "done"

		_, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Status: &inProgress,
		})
		require.NoError(t, err)

		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision + 1, Status: &done,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "invalid_transition", formationErr.Reason)
	})

	t.Run("reopen and accept are refused on this route", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		inProgress := "in_progress"
		awaiting := "awaiting_acceptance"
		done := "done"

		_, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Status: &inProgress,
		})
		require.NoError(t, err)

		claimed, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision + 1, Status: &awaiting,
		})
		require.NoError(t, err, "the assignee's own completion claim is this route's job")

		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: claimed.Version, Status: &done,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "invalid_transition", formationErr.Reason, "accepting is the accept route's job, not this one")
	})

	t.Run("checklist read-only refuses every mutation", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		note := "hello"

		_, err := s.formations.UpdateLifecycle(context.Background(), formation.UID, model.LifecycleCompleted, formation.Revision)
		require.NoError(t, err)

		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Note: &note,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "Conflict", formationErr.Name)
		assert.Equal(t, "checklist_read_only", formationErr.Reason)
	})

	t.Run("evidence link with an unsafe scheme is refused", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		unsafe := "javascript:alert(1)"

		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
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
		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, Assignee: &outsider,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "assignee_not_on_project", formationErr.Reason)
	})

	t.Run("assignee holding a project grant succeeds", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		projects := mock.NewProjectReader()
		projects.SetSettings(formation.ProjectUID, &port.ProjectSettings{
			ProjectUID: formation.ProjectUID, Writers: []string{"alice"},
		})
		s.projects = projects

		alice := "alice"
		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
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

		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
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

		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
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

		set, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, DueDate: &due,
		})
		require.NoError(t, err)
		require.NotNil(t, set.DueDate)
		assert.Equal(t, due, *set.DueDate)

		empty := ""
		cleared, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision + 1, DueDate: &empty,
		})
		require.NoError(t, err)
		assert.Nil(t, cleared.DueDate, "an empty string must clear due_date")
	})

	t.Run("malformed due_date is refused", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		bad := "not-a-date"

		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision, DueDate: &bad,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "BadRequest", formationErr.Name)
		assert.Equal(t, "due_date_invalid", formationErr.Reason)
	})

	t.Run("re-patching an already-skipped item with an empty skip_reason is refused", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		skipped := "skipped"
		reason := "blocked by legal review"

		afterSkip, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: &skipped, SkipReason: &reason,
		})
		require.NoError(t, err)

		emptyReason := ""
		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: afterSkip.Version,
			Status: &skipped, SkipReason: &emptyReason,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "skip_reason_required", formationErr.Reason)
	})
}
