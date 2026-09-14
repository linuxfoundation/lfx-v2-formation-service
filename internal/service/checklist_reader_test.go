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
	goa "goa.design/goa/v3/pkg"

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
		// Mirrors what Expander snapshots at creation: GetFormation now reads
		// sections from here, not from a live join to the template.
		Sections: []model.FormationSection{{Key: "sec-1", Title: "Section One"}},
	})
	require.NoError(t, err)

	_, err = items.InsertMany(context.Background(), []*model.Item{
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
	_, err := s.items.InsertMany(context.Background(), []*model.Item{
		{
			FormationUID: formation.UID,
			ItemKey:      "gate-1",
			SectionKey:   "sec-1",
			Title:        "Gating Item",
			Status:       status,
			Gate:         true,
		},
	})
	require.NoError(t, err)
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

// The item filter. Only the first of these is about returning the right
// entries; the rest are about every way of asking wrongly being reported as
// the specific thing that went wrong, which is what keeps an empty page
// meaning exactly "no history".
func TestGetFormationActivityFilteredByItem(t *testing.T) {
	// An item that exists and has no history is an empty page,
	// not an error. This is the case the existence check must not swallow:
	// having added a not-found for an unknown item, the temptation is to make
	// "nothing to show" the same answer.
	t.Run("an item with no history returns an empty page rather than an error", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		item := seedActivityItem(t, s, formation, "quiet_item")
		// Unrelated history, so an empty result is the filter working rather
		// than an empty feed.
		require.NoError(t, s.activity.Append(context.Background(), &model.ActivityEntry{
			FormationUID: formation.UID,
			Actor:        "someone-else",
			SetBy:        model.SetByUser,
			Action:       "status_changed",
		}))

		result, err := s.GetFormationActivity(context.Background(), activityPayload(formation.ProjectUID, item.UID.String()))

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Empty(t, result.Entries)
	})

	t.Run("a filtered read returns that item's entries and no others", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		wanted := seedActivityItem(t, s, formation, "wanted_item")
		other := seedActivityItem(t, s, formation, "other_item")
		for _, itemUID := range []*uuid.UUID{&wanted.UID, &other.UID, nil, &other.UID} {
			require.NoError(t, s.activity.Append(context.Background(), &model.ActivityEntry{
				FormationUID: formation.UID,
				ItemUID:      itemUID,
				Actor:        "actor",
				SetBy:        model.SetByUser,
				Action:       "status_changed",
			}))
		}

		result, err := s.GetFormationActivity(context.Background(), activityPayload(formation.ProjectUID, wanted.UID.String()))

		require.NoError(t, err)
		require.Len(t, result.Entries, 1)
		require.NotNil(t, result.Entries[0].ItemUID)
		assert.Equal(t, wanted.UID.String(), *result.Entries[0].ItemUID)
	})

	// The feature's one real authorization hazard. The
	// natural shape of the change — "accept an item identifier, return its
	// entries" — becomes a route to any item's history in any project if the
	// formation predicate is dropped once an identifier is present. Asserted
	// on the absence of entries as well as the status, because a status-only
	// assertion passes against a route that answers 404 and leaks a body.
	t.Run("an item in another formation is not found and returns no entries", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		elsewhere := seedForeignFormationItem(t, s)
		require.NoError(t, s.activity.Append(context.Background(), &model.ActivityEntry{
			FormationUID: elsewhere.FormationUID,
			ItemUID:      &elsewhere.UID,
			Actor:        "actor-in-another-project",
			SetBy:        model.SetByUser,
			Action:       "status_changed",
		}))

		result, err := s.GetFormationActivity(context.Background(), activityPayload(formation.ProjectUID, elsewhere.UID.String()))

		assert.Nil(t, result, "a cross-formation request must return no entries at all, not a 404 beside a body")
		var notFound *svc.NotFoundError
		require.ErrorAs(t, err, &notFound)
	})

	// Two assertions that pull in opposite
	// directions and are both required. The two item cases must be
	// indistinguishable from each other, or this route is an existence oracle
	// for other projects' items. The item case and the formation case must be
	// distinguishable from each other, or a caller cannot tell which of the
	// two it got. Message is the only thing carrying either, so an unasserted
	// message is an unenforced contract.
	t.Run("the two item not-founds are identical and both differ from the formation not-found", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		elsewhere := seedForeignFormationItem(t, s)

		_, errElsewhere := s.GetFormationActivity(context.Background(), activityPayload(formation.ProjectUID, elsewhere.UID.String()))
		_, errNowhere := s.GetFormationActivity(context.Background(), activityPayload(formation.ProjectUID, uuid.NewString()))
		_, errNoFormation := s.GetFormationActivity(context.Background(), activityPayload("no-such-project", uuid.NewString()))

		var elsewhereErr, nowhereErr, noFormationErr *svc.NotFoundError
		require.ErrorAs(t, errElsewhere, &elsewhereErr)
		require.ErrorAs(t, errNowhere, &nowhereErr)
		require.ErrorAs(t, errNoFormation, &noFormationErr)

		// Whole rendered errors, not just their codes: comparing status alone
		// would pass on two different bodies.
		assert.Equal(t, *nowhereErr, *elsewhereErr,
			"an item in a project the caller cannot see must read exactly like an item that exists nowhere")

		assert.Equal(t, itemNotFoundMessage, elsewhereErr.Message)
		assert.Equal(t, formationNotFoundMessage, noFormationErr.Message)
		assert.NotEqual(t, noFormationErr.Message, elsewhereErr.Message,
			"the two not-found cases are distinguished by message alone, so they cannot share one")
	})

	// A missing formation is reported before the item is looked at, so a
	// caller naming a real item under a project with no checklist is told
	// about the checklist. Otherwise the item check would answer first and
	// report the wrong one of the two.
	t.Run("no formation for the project is reported ahead of the item", func(t *testing.T) {
		s, _ := newChecklistTestService(t)

		_, err := s.GetFormationActivity(context.Background(), activityPayload("no-such-project", uuid.NewString()))

		var notFound *svc.NotFoundError
		require.ErrorAs(t, err, &notFound)
		assert.Equal(t, formationNotFoundMessage, notFound.Message)
	})

	// The same rejection at the service boundary. Unreachable over HTTP, where decode
	// rejects it first (asserted through the transport in
	// TestGetFormationActivityRejectsAMalformedItemUID), but a direct caller
	// must not get the unfiltered feed and must not get a server error.
	t.Run("a malformed item reference is a client error and never an unfiltered feed", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		require.NoError(t, s.activity.Append(context.Background(), &model.ActivityEntry{
			FormationUID: formation.UID,
			Actor:        "actor",
			SetBy:        model.SetByUser,
			Action:       "status_changed",
		}))

		result, err := s.GetFormationActivity(context.Background(), activityPayload(formation.ProjectUID, "not-a-uuid"))

		assert.Nil(t, result, "a malformed reference must not fall back to the unfiltered feed")
		var serviceErr *goa.ServiceError
		require.ErrorAs(t, err, &serviceErr)
		assert.False(t, serviceErr.Fault, "a malformed parameter is the caller's error, so it must not encode as a 500")
	})

	// Filtered paging at the service boundary. The repository's own paging is
	// covered against Postgres; this is the route carrying the cursor through
	// unchanged when a filter is present.
	t.Run("a filtered read pages with the feed's existing cursor semantics", func(t *testing.T) {
		s, formation := newChecklistTestService(t)
		item := seedActivityItem(t, s, formation, "busy_item")
		const total = 5
		for i := range total {
			require.NoError(t, s.activity.Append(context.Background(), &model.ActivityEntry{
				FormationUID: formation.UID,
				ItemUID:      &item.UID,
				Actor:        fmt.Sprintf("actor-%d", i),
				SetBy:        model.SetByUser,
				Action:       "status_changed",
			}))
			// An unrelated entry between each, so a page that ignored the
			// filter would come back with twice as many.
			require.NoError(t, s.activity.Append(context.Background(), &model.ActivityEntry{
				FormationUID: formation.UID,
				Actor:        "noise",
				SetBy:        model.SetByUser,
				Action:       "status_changed",
			}))
		}

		var seen []string
		cursor := ""
		for page := 0; page < total+1; page++ {
			p := activityPayload(formation.ProjectUID, item.UID.String())
			p.Limit = 2
			if cursor != "" {
				p.Cursor = &cursor
			}
			result, err := s.GetFormationActivity(context.Background(), p)
			require.NoError(t, err)
			for _, e := range result.Entries {
				require.NotNil(t, e.ItemUID, "a filtered page must not carry an entry with no item")
				assert.Equal(t, item.UID.String(), *e.ItemUID)
				seen = append(seen, e.Actor)
			}
			if result.NextCursor == nil || *result.NextCursor == "" || len(result.Entries) == 0 {
				break
			}
			cursor = *result.NextCursor
		}

		require.Len(t, seen, total)
		assert.Equal(t, []string{"actor-4", "actor-3", "actor-2", "actor-1", "actor-0"}, seen)
	})
}

// The consumer that sends no item reference must not notice this change.
//
// The comparison is against an unfiltered read of the same repository, which
// is the method the route itself calls — so this pins the route's own
// behaviour (that it passes no filter, preserves order and carries the cursor
// through untouched) and not the repository's. A bug in the double's
// no-filter path would break both sides alike and this would still pass. The
// independent version of this assertion, reading the table directly, is
// TestListUnfilteredFeedIsUnchangedByTheFilter in the postgres package; the
// two are worth having separately because only that one can see the rows.
func TestGetFormationActivityWithoutAnItemIsUnchanged(t *testing.T) {
	s, formation := newChecklistTestService(t)
	item := seedActivityItem(t, s, formation, "an_item")
	for i := range 7 {
		entry := &model.ActivityEntry{
			FormationUID: formation.UID,
			Actor:        fmt.Sprintf("actor-%d", i),
			SetBy:        model.SetByUser,
			Action:       "status_changed",
		}
		// A mix of item-bearing and formation-level entries, because the
		// unfiltered feed carries both and must keep carrying both.
		if i%2 == 0 {
			entry.ItemUID = &item.UID
		}
		require.NoError(t, s.activity.Append(context.Background(), entry))
	}

	baseline, baselineCursor, err := s.activity.List(context.Background(), formation.UID, nil, "", defaultActivityPageLimit)
	require.NoError(t, err)
	require.NotEmpty(t, baseline)

	result, err := s.GetFormationActivity(context.Background(), &svc.GetFormationActivityPayload{ProjectUID: formation.ProjectUID})

	require.NoError(t, err)
	require.NotNil(t, result.NextCursor)
	assert.Equal(t, baselineCursor, *result.NextCursor)
	require.Len(t, result.Entries, len(baseline))

	withoutItem := 0
	for i, want := range baseline {
		assert.Equal(t, want.ULID, result.Entries[i].Ulid, "entry %d moved", i)
		assert.Equal(t, want.Actor, result.Entries[i].Actor, "entry %d moved", i)
		if want.ItemUID == nil {
			withoutItem++
			assert.Nil(t, result.Entries[i].ItemUID, "entry %d gained an item reference", i)
		}
	}
	assert.NotZero(t, withoutItem, "no formation-level entry in the comparison, so it does not cover them")
}

// activityPayload builds the route's payload with an item filter set, which
// is a pointer on the generated type and so awkward to write inline.
func activityPayload(projectUID, itemUID string) *svc.GetFormationActivityPayload {
	return &svc.GetFormationActivityPayload{ProjectUID: projectUID, ItemUID: &itemUID}
}

// seedActivityItem adds one item to the formation and returns it, for tests
// that need a real item UID to filter on.
func seedActivityItem(t *testing.T, s *Service, formation *model.Formation, key string) *model.Item {
	t.Helper()
	// UID set here rather than left to InsertMany: the real repository
	// writes it back onto the argument, but the in-memory double inserts a
	// clone, so a caller that needs the UID afterwards has to supply it.
	item := &model.Item{
		UID:          uuid.New(),
		FormationUID: formation.UID,
		ItemKey:      key,
		SectionKey:   "sec-1",
		Title:        key,
		Status:       model.StatusNotStarted,
	}
	_, err := s.items.InsertMany(context.Background(), []*model.Item{item})
	require.NoError(t, err)
	return item
}

// seedForeignFormationItem adds a second formation with an item on it, so a
// request can name a real item that belongs to a project the caller did not
// ask about.
func seedForeignFormationItem(t *testing.T, s *Service) *model.Item {
	t.Helper()
	other, err := s.formations.Create(context.Background(), &model.Formation{
		ProjectUID:      "project-2",
		TemplateUID:     uuid.New(),
		TemplateVersion: 1,
	})
	require.NoError(t, err)
	item := &model.Item{
		UID:          uuid.New(),
		FormationUID: other.UID,
		ItemKey:      "item_in_another_project",
		SectionKey:   "sec-1",
		Title:        "Item in another project",
		Status:       model.StatusNotStarted,
	}
	_, err = s.items.InsertMany(context.Background(), []*model.Item{item})
	require.NoError(t, err)
	return item
}

// failingItemRepository is a port.ItemRepository double whose every method
// returns err, for exercising the service's dependency-error paths.
type failingItemRepository struct{ err error }

func (f failingItemRepository) InsertMany(context.Context, []*model.Item) ([]string, error) {
	return nil, f.err
}
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
func (f failingActivityRepository) List(
	context.Context, uuid.UUID, *uuid.UUID, string, int,
) ([]*model.ActivityEntry, string, error) {
	return nil, "", f.err
}
