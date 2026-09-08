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

	t.Run("a null sub_items entry is refused, not a panic", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)

		// Goa's generated validator steps over nil elements without
		// reporting them, so `"sub_items":[null]` reaches the service as a
		// live nil. It used to reach a field access and take the handler
		// down with a nil dereference.
		_, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
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

	t.Run("an already-skipped item cannot have its reason cleared without a status", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		skipped, reason := "skipped", "not applicable to this project"

		// Get the item into skipped-with-a-reason first.
		first, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: &skipped, SkipReason: &reason,
		})
		require.NoError(t, err)

		// Now clear the reason alone. The old check only ran when status was
		// present, so this reached the skip_needs_reason constraint and came
		// back as a 500.
		empty := ""
		_, err = s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: first.Version,
			SkipReason: &empty,
		})

		require.Error(t, err)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "BadRequest", formationErr.Name)
		assert.Equal(t, "skip_reason_required", formationErr.Reason)
	})

	t.Run("a skipped item can still be patched on an unrelated field", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		skipped, reason := "skipped", "not applicable to this project"

		first, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: &skipped, SkipReason: &reason,
		})
		require.NoError(t, err)

		// Guards the cost of checking the invariant against resolved values
		// rather than the fields present: the reason falls back to the stored
		// one, so a note-only PATCH must not be read as clearing it.
		note := "still worth recording why"
		got, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: first.Version,
			Note: &note,
		})

		require.NoError(t, err)
		assert.Equal(t, "skipped", got.Status)
	})

	t.Run("a whitespace-only skip reason is refused", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		skipped, blank := "skipped", "   "

		// Postgres compares btrim(skip_reason), so this passed an == "" test
		// in the service and failed only in the database.
		_, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: &skipped, SkipReason: &blank,
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
		_, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
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
		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
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
		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey,
			IfMatch:  itemOne.Revision,
			Assignee: &outsider,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "checklist_read_only", formationErr.Reason)
	})

	// The assignee check sits after the status rules, so a request that is wrong
	// about both is answered on the transition. This is the case the first
	// attempt at moving the check out of the transaction got wrong: it hoisted
	// the refusal to the front and turned this 409 into a 400.
	t.Run("an invalid transition outranks a bad assignee", func(t *testing.T) {
		s, formation, itemOne, _ := newItemMutatorTestService(t)
		projects := mock.NewProjectReader()
		projects.SetSettings(formation.ProjectUID, &port.ProjectSettings{
			ProjectUID: formation.ProjectUID, Writers: []string{"alice"},
		})
		s.projects = projects

		outsider := "mallory"
		done := string(model.StatusDone)
		result, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
			ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
			Status: &done, Assignee: &outsider,
		})

		require.Nil(t, result)
		var formationErr *svc.FormationError
		require.ErrorAs(t, err, &formationErr)
		assert.Equal(t, "invalid_transition", formationErr.Reason)
		assert.Equal(t, "Conflict", formationErr.Name)
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

// The activity feed names an entry after the field the caller led with, so the
// precedence between cases is the behaviour worth pinning, not just the
// mapping. skip_reason is deliberately last: it was added after the others and
// keeping it there left every existing combination's label unchanged.
func TestMutationAction(t *testing.T) {
	str := func(s string) *string { return &s }

	tests := []struct {
		name string
		p    *svc.UpdateItemPayload
		want string
	}{
		{"status alone", &svc.UpdateItemPayload{Status: str("done")}, "status_changed"},
		{"assignee alone", &svc.UpdateItemPayload{Assignee: str("someone")}, "assignee_changed"},
		{"evidence link alone", &svc.UpdateItemPayload{EvidenceLink: str("https://example.test")}, "evidence_link_changed"},
		{"due date alone", &svc.UpdateItemPayload{DueDate: str("2026-01-01")}, "due_date_changed"},
		{"note alone", &svc.UpdateItemPayload{Note: str("a note")}, "note_changed"},
		{"sub items alone", &svc.UpdateItemPayload{SubItems: []*svc.FormationSubItemUpdate{}}, "sub_items_changed"},
		{"skip reason alone", &svc.UpdateItemPayload{SkipReason: str("not applicable")}, "skip_reason_changed"},
		{
			"status wins over a co-submitted skip reason",
			&svc.UpdateItemPayload{Status: str("skipped"), SkipReason: str("not applicable")},
			"status_changed",
		},
		{
			"sub items win over a co-submitted skip reason",
			&svc.UpdateItemPayload{SubItems: []*svc.FormationSubItemUpdate{}, SkipReason: str("not applicable")},
			"sub_items_changed",
		},
		{"nothing recognised", &svc.UpdateItemPayload{}, "item_updated"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mutationAction(tc.p))
		})
	}
}

func TestUpdateItemRefusesANoOp(t *testing.T) {
	s, formation, itemOne, _ := newItemMutatorTestService(t)

	// Update always increments the revision, so applying this would have
	// invalidated other clients' if_match and logged a change that never
	// happened.
	_, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
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
	_, err := s.UpdateItem(context.Background(), &svc.UpdateItemPayload{
		ProjectUID: formation.ProjectUID, ItemKey: itemOne.ItemKey, IfMatch: itemOne.Revision,
		DueDate: &yearZero,
	})

	require.Error(t, err)
	var formationErr *svc.FormationError
	require.ErrorAs(t, err, &formationErr)
	assert.Equal(t, "BadRequest", formationErr.Name)
	assert.Equal(t, "due_date_invalid", formationErr.Reason)
}
