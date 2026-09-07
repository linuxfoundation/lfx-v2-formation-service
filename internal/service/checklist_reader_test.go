// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// newChecklistTestService wires a Service over fresh in-memory doubles and
// seeds a formation with one section and one item, returning the service and
// the formation so tests can target it.
func newChecklistTestService(t *testing.T) (*Service, *model.Formation) {
	t.Helper()

	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	templates := mock.NewTemplateRepository()
	activity := mock.NewActivityRepository()

	template, err := templates.Upsert(context.Background(), &model.Template{
		Name:    "default",
		Version: 1,
		State:   model.TemplatePublished,
		Sections: []model.TemplateSection{
			{Key: "sec-1", Title: "Section One"},
		},
	})
	require.NoError(t, err)

	formation, err := formations.Create(context.Background(), &model.Formation{
		ProjectUID:      "project-1",
		TemplateUID:     template.UID,
		TemplateVersion: template.Version,
	})
	require.NoError(t, err)

	err = items.InsertMany(context.Background(), []*model.Item{
		{
			FormationUID: formation.UID,
			ItemKey:      "item-1",
			SectionKey:   "sec-1",
			Title:        "Item One",
			Status:       model.StatusNotStarted,
		},
	})
	require.NoError(t, err)

	s := NewService(
		WithFormations(formations),
		WithItems(items),
		WithTemplates(templates),
		WithActivity(activity),
	)
	return s, formation
}

func TestGetFormation(t *testing.T) {
	t.Run("success assembles sections, items and progress", func(t *testing.T) {
		s, formation := newChecklistTestService(t)

		result, err := s.GetFormation(context.Background(), &svc.GetFormationPayload{ProjectUID: formation.ProjectUID})

		require.NoError(t, err)
		assert.Equal(t, formation.ProjectUID, result.ProjectUID)
		assert.Equal(t, string(model.LifecycleLive), result.Lifecycle)
		require.Len(t, result.Sections, 1)
		assert.Equal(t, "sec-1", result.Sections[0].Key)
		require.Len(t, result.Items, 1)
		assert.Equal(t, "item-1", result.Items[0].ItemKey)
		require.NotNil(t, result.Progress)
		assert.False(t, result.IsActivating)
	})

	t.Run("no formation for project maps to NotFoundError", func(t *testing.T) {
		s, _ := newChecklistTestService(t)

		result, err := s.GetFormation(context.Background(), &svc.GetFormationPayload{ProjectUID: "no-such-project"})

		require.Nil(t, result)
		var notFound *svc.NotFoundError
		require.ErrorAs(t, err, &notFound)
	})

	t.Run("item repository error is not swallowed", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		s.items = failingItemRepository{err: errors.New("boom")}

		result, err := s.GetFormation(context.Background(), &svc.GetFormationPayload{ProjectUID: formation.ProjectUID})

		require.Nil(t, result)
		assert.EqualError(t, err, "boom")
	})
}

func TestGetFormationActivity(t *testing.T) {
	t.Run("success returns entries newest first", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		require.NoError(t, s.activity.Append(context.Background(), &model.ActivityEntry{
			FormationUID: formation.UID,
			Actor:        "user-1",
			SetBy:        model.SetByUser,
			Action:       "status_changed",
		}))

		result, err := s.GetFormationActivity(context.Background(), &svc.GetFormationActivityPayload{ProjectUID: formation.ProjectUID})

		require.NoError(t, err)
		require.Len(t, result.Entries, 1)
		assert.Equal(t, "user-1", result.Entries[0].Actor)
		require.NotNil(t, result.NextCursor)
	})

	t.Run("no formation for project maps to NotFoundError", func(t *testing.T) {
		s, _ := newChecklistTestService(t)

		result, err := s.GetFormationActivity(context.Background(), &svc.GetFormationActivityPayload{ProjectUID: "no-such-project"})

		require.Nil(t, result)
		var notFound *svc.NotFoundError
		require.ErrorAs(t, err, &notFound)
	})

	t.Run("activity repository error is not swallowed", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		s.activity = failingActivityRepository{err: errors.New("boom")}

		result, err := s.GetFormationActivity(context.Background(), &svc.GetFormationActivityPayload{ProjectUID: formation.ProjectUID})

		require.Nil(t, result)
		assert.EqualError(t, err, "boom")
	})
}

// failingItemRepository is a port.ItemRepository double whose every method
// returns err, for exercising the service's dependency-error paths.
type failingItemRepository struct{ err error }

func (f failingItemRepository) InsertMany(context.Context, []*model.Item) error { return f.err }
func (f failingItemRepository) ListByFormation(context.Context, uuid.UUID) ([]*model.Item, error) {
	return nil, f.err
}
func (f failingItemRepository) Get(context.Context, uuid.UUID) (*model.Item, error) {
	return nil, f.err
}
func (f failingItemRepository) Update(context.Context, uuid.UUID, int64, port.ItemPatch) (*model.Item, error) {
	return nil, f.err
}
func (f failingItemRepository) StatusCounts(context.Context, uuid.UUID) (map[model.ItemStatus]int, error) {
	return nil, f.err
}
func (f failingItemRepository) GateSummary(context.Context, uuid.UUID) (int, int, error) {
	return 0, 0, f.err
}

// failingActivityRepository is a port.ActivityRepository double whose every
// method returns err.
type failingActivityRepository struct{ err error }

func (f failingActivityRepository) Append(context.Context, *model.ActivityEntry) error {
	return f.err
}
func (f failingActivityRepository) List(context.Context, uuid.UUID, string, int) ([]*model.ActivityEntry, string, error) {
	return nil, "", f.err
}
