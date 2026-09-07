// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
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

	// is_activating is the readiness flag screen two drives its activation
	// banner from, and it is true only when all three inputs line up: at
	// least one gating item, none of them outstanding, and an announcement
	// date on the project. Each subtest below removes exactly one of those.
	t.Run("is_activating true when gates are all done and an announcement date is set", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		seedGateItem(t, s, formation, model.StatusDone)
		projects := mock.NewProjectReader()
		projects.SetSettings(formation.ProjectUID, &port.ProjectSettings{AnnouncementDate: strPtr("2026-01-15")})
		s.projects = projects

		result, err := s.GetFormation(context.Background(), &svc.GetFormationPayload{ProjectUID: formation.ProjectUID})

		require.NoError(t, err)
		assert.True(t, result.IsActivating)
	})

	t.Run("is_activating false when a gating item is still outstanding", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		seedGateItem(t, s, formation, model.StatusInProgress)
		projects := mock.NewProjectReader()
		projects.SetSettings(formation.ProjectUID, &port.ProjectSettings{AnnouncementDate: strPtr("2026-01-15")})
		s.projects = projects

		result, err := s.GetFormation(context.Background(), &svc.GetFormationPayload{ProjectUID: formation.ProjectUID})

		require.NoError(t, err)
		assert.False(t, result.IsActivating)
	})

	t.Run("is_activating false when gates are done but no announcement date is set", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		seedGateItem(t, s, formation, model.StatusDone)
		s.projects = mock.NewProjectReader()

		result, err := s.GetFormation(context.Background(), &svc.GetFormationPayload{ProjectUID: formation.ProjectUID})

		require.NoError(t, err)
		assert.False(t, result.IsActivating)
	})

	// A skipped gating item was excused, not completed, so it must still
	// read as outstanding — otherwise skipping a gate would activate the
	// project.
	t.Run("is_activating false when a gating item was skipped rather than done", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		seedGateItem(t, s, formation, model.StatusSkipped)
		projects := mock.NewProjectReader()
		projects.SetSettings(formation.ProjectUID, &port.ProjectSettings{AnnouncementDate: strPtr("2026-01-15")})
		s.projects = projects

		result, err := s.GetFormation(context.Background(), &svc.GetFormationPayload{ProjectUID: formation.ProjectUID})

		require.NoError(t, err)
		assert.False(t, result.IsActivating)
	})

	t.Run("progress counts every status from the loaded items", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		seedGateItem(t, s, formation, model.StatusDone)

		result, err := s.GetFormation(context.Background(), &svc.GetFormationPayload{ProjectUID: formation.ProjectUID})

		require.NoError(t, err)
		require.NotNil(t, result.Progress)
		assert.Equal(t, 1, result.Progress.NotStarted)
		assert.Equal(t, 1, result.Progress.Done)
		assert.Equal(t, 0, result.Progress.Skipped)
	})
}

// seedGateItem adds one gating item in the given status to the formation, so
// readiness tests can control the gate summary.
func seedGateItem(t *testing.T, s *Service, formation *model.Formation, status model.ItemStatus) {
	t.Helper()
	require.NoError(t, s.items.InsertMany(context.Background(), []*model.Item{
		{
			FormationUID: formation.UID,
			ItemKey:      "gate-1",
			SectionKey:   "sec-1",
			Title:        "Gating Item",
			Status:       status,
			Gate:         true,
		},
	}))
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

	// Walking the feed with the cursor the previous page handed back must
	// visit every entry exactly once and in newest-first order. A cursor
	// that failed to advance would loop on page one forever, and one that
	// advanced too far would silently drop entries — neither shows up in a
	// single-page test.
	t.Run("cursor paging walks every entry once, newest first", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		const total = 5
		for i := range total {
			require.NoError(t, s.activity.Append(context.Background(), &model.ActivityEntry{
				FormationUID: formation.UID,
				Actor:        fmt.Sprintf("user-%d", i),
				SetBy:        model.SetByUser,
				Action:       "status_changed",
			}))
		}

		var seen []string
		cursor := ""
		for page := 0; page < total+1; page++ {
			p := &svc.GetFormationActivityPayload{ProjectUID: formation.ProjectUID, Limit: 2}
			if cursor != "" {
				p.Cursor = &cursor
			}
			result, err := s.GetFormationActivity(context.Background(), p)
			require.NoError(t, err)

			for _, e := range result.Entries {
				seen = append(seen, e.Actor)
			}
			if result.NextCursor == nil || *result.NextCursor == "" || len(result.Entries) == 0 {
				break
			}
			cursor = *result.NextCursor
		}

		// Newest first: user-4 was appended last, so it leads.
		require.Len(t, seen, total)
		assert.Equal(t, []string{"user-4", "user-3", "user-2", "user-1", "user-0"}, seen)
	})

	t.Run("limit of zero falls back to the default page size", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		for i := range defaultActivityPageLimit + 5 {
			require.NoError(t, s.activity.Append(context.Background(), &model.ActivityEntry{
				FormationUID: formation.UID,
				Actor:        fmt.Sprintf("user-%d", i),
				SetBy:        model.SetByUser,
				Action:       "status_changed",
			}))
		}

		result, err := s.GetFormationActivity(context.Background(), &svc.GetFormationActivityPayload{
			ProjectUID: formation.ProjectUID,
			Limit:      0,
		})

		require.NoError(t, err)
		assert.Len(t, result.Entries, defaultActivityPageLimit)
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
func (f failingItemRepository) GetByKey(context.Context, uuid.UUID, string) (*model.Item, error) {
	return nil, f.err
}
func (f failingItemRepository) Update(context.Context, uuid.UUID, int64, port.ItemPatch) (*model.Item, error) {
	return nil, f.err
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
