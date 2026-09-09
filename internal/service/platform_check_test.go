// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// platformFixture seeds a checklist with one platform item in a chosen status, and
// a checker whose lookup can be set per test.
//
// The lookup is injected rather than taken from the package-level registry,
// because that registry is deliberately empty — no owning service answers a
// project-scoped lookup yet. Testing only against the empty registry would leave
// the forward-only rule, which is the whole point of this file, unexercised until
// the first lookup lands.
func platformFixture(
	t *testing.T, status model.ItemStatus, lookup platformLookup,
) (*PlatformChecker, *platformRepos, *model.Formation) {
	t.Helper()

	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	templates := mock.NewTemplateRepository()
	activity := mock.NewActivityRepository()
	uow := mock.NewUnitOfWork(formations, items, activity, templates)

	formation, err := formations.Create(context.Background(), &model.Formation{
		ProjectUID: "project-1", Lifecycle: model.LifecycleLive,
	})
	require.NoError(t, err)

	_, err = items.InsertMany(context.Background(), []*model.Item{{
		FormationUID:  formation.UID,
		ItemKey:       "tsc_kickoff",
		SectionKey:    "sec-1",
		Title:         "Charter the TSC",
		Status:        status,
		StatusSource:  model.SourcePlatform,
		PlatformCheck: &model.PlatformCheck{ResourceType: "committee", MinCount: 1},
		Note:          "a person wrote this",
	}})
	require.NoError(t, err)

	checker := &PlatformChecker{uow: uow, lookups: map[string]platformLookup{}}
	if lookup != nil {
		checker.lookups["committee"] = lookup
	}
	return checker, &platformRepos{
		formations: formations, items: items, activity: activity,
	}, formation
}

// platformRepos are the doubles a test asserts against, handed back rather than
// reached through the checker: the checker holds a unit of work, not the
// repositories, and a test that reached inside it would be asserting on wiring.
type platformRepos struct {
	formations *mock.FormationRepository
	items      *mock.ItemRepository
	activity   *mock.ActivityRepository
}

func foundCommittee(_ context.Context, _ string) (*model.ResolvedRef, error) {
	return &model.ResolvedRef{Type: "committee", UID: "committee-1"}, nil
}

// The invariant this file exists for. A platform check may move an item to done
// and may do nothing else — so every status a person could have set has to come
// out of a pass unchanged, even when the lookup says the resource exists.
//
// Table-driven over every status rather than spot-checked, because the failure
// mode is a check quietly reversing somebody's judgment, and the status most
// likely to be got wrong is whichever one nobody thought to list.
func TestAPlatformCheckNeverRegressesAStatusAPersonSet(t *testing.T) {
	// done and skipped must survive a pass that would otherwise advance the item.
	for _, status := range []model.ItemStatus{model.StatusDone, model.StatusSkipped} {
		t.Run(string(status), func(t *testing.T) {
			checker, repos, formation := platformFixture(t, status, foundCommittee)

			report, err := checker.ResolveFor(context.Background(), "project-1")
			require.NoError(t, err)
			assert.Equal(t, 0, report.Advanced, "the check moved an item it must not touch")
			assert.Equal(t, 1, report.Unchanged)

			after, err := repos.items.GetByKey(context.Background(), formation.UID, "tsc_kickoff")
			require.NoError(t, err)
			assert.Equal(t, status, after.Status, "a check changed a status a person set")
			assert.Equal(t, "a person wrote this", after.Note, "a check overwrote a person's note")
		})
	}
}

// The only move a check may make, and it may make it from any unfinished status —
// including blocked, which is the case worth being explicit about: a person
// marking a row blocked did so for a reason the platform cannot see, so advancing
// it is a judgment call. The rule is that the platform reports facts and the fact
// outranks the note, but the note survives.
func TestAPlatformCheckAdvancesAnUnfinishedItemToDone(t *testing.T) {
	for _, status := range []model.ItemStatus{
		model.StatusNotStarted, model.StatusInProgress, model.StatusBlocked,
	} {
		t.Run(string(status), func(t *testing.T) {
			checker, repos, formation := platformFixture(t, status, foundCommittee)

			report, err := checker.ResolveFor(context.Background(), "project-1")
			require.NoError(t, err)
			assert.Equal(t, 1, report.Advanced)

			after, err := repos.items.GetByKey(context.Background(), formation.UID, "tsc_kickoff")
			require.NoError(t, err)
			assert.Equal(t, model.StatusDone, after.Status)
			require.NotNil(t, after.ResolvedRef, "the resolved reference is what turns Create into Open")
			assert.Equal(t, "committee-1", after.ResolvedRef.UID)
			assert.Equal(t, "a person wrote this", after.Note, "advancing an item erased its note")
		})
	}
}

// Reaching done takes no acceptance step. There is nothing for a second person to
// confirm: the platform is reporting a fact rather than attesting to its own work,
// which is what the two-person rule guards against.
func TestAPlatformCheckReachesDoneWithoutAwaitingAcceptance(t *testing.T) {
	checker, repos, formation := platformFixture(t, model.StatusInProgress, foundCommittee)

	_, err := checker.ResolveFor(context.Background(), "project-1")
	require.NoError(t, err)

	after, err := repos.items.GetByKey(context.Background(), formation.UID, "tsc_kickoff")
	require.NoError(t, err)
	assert.Equal(t, model.StatusDone, after.Status,
		"a platform item stopped at awaiting_acceptance, which would need a person to accept a fact")
}

// The move is attributed to the system. Naming a person for a change they did not
// make would be worse than recording nothing, because this feed is what the
// acceptance rule gets audited against.
func TestAnAdvancedItemIsAttributedToTheSystem(t *testing.T) {
	checker, repos, formation := platformFixture(t, model.StatusInProgress, foundCommittee)

	_, err := checker.ResolveFor(context.Background(), "project-1")
	require.NoError(t, err)

	entries, _, err := repos.activity.List(context.Background(), formation.UID, "", 50)
	require.NoError(t, err)

	var found bool
	for _, entry := range entries {
		if entry.Action != "platform_check_resolved" {
			continue
		}
		found = true
		assert.Equal(t, model.SetBySystem, entry.SetBy)
		assert.Equal(t, actorSystem, entry.Actor)
	}
	assert.True(t, found, "advancing an item recorded no activity entry")
}

// A resource that does not exist yet leaves the item alone. That is the ordinary
// state of a row still to be done, and it must not be confused with the platform
// being unable to answer.
func TestAMissingResourceIsPendingRatherThanUnsupported(t *testing.T) {
	notYet := func(_ context.Context, _ string) (*model.ResolvedRef, error) {
		return nil, domain.ErrNotFound
	}
	checker, repos, formation := platformFixture(t, model.StatusNotStarted, notYet)

	report, err := checker.ResolveFor(context.Background(), "project-1")
	require.NoError(t, err)
	assert.Equal(t, 1, report.Pending)
	assert.Equal(t, 0, report.Unsupported,
		"a resource that does not exist yet was reported as something nobody can answer for")
	assert.Equal(t, 0, report.Advanced)

	after, err := repos.items.GetByKey(context.Background(), formation.UID, "tsc_kickoff")
	require.NoError(t, err)
	assert.Equal(t, model.StatusNotStarted, after.Status)
}

// The state the platform is actually in today: no owning service answers a
// project-scoped lookup for any of the three resource types the template marks
// platform, so every one of those rows is left to a person.
//
// Asserted rather than assumed, so that registering the first real lookup is a
// deliberate change with a failing test in front of it.
func TestWithNoRegisteredLookupTheRowIsLeftToAPerson(t *testing.T) {
	checker, repos, formation := platformFixture(t, model.StatusNotStarted, nil)

	report, err := checker.ResolveFor(context.Background(), "project-1")
	require.NoError(t, err)
	assert.Equal(t, 1, report.Unsupported)
	assert.Equal(t, 0, report.Advanced)
	assert.Equal(t, 0, report.Pending,
		"an unanswerable row was counted as pending, which reads as though it were being waited on")

	after, err := repos.items.GetByKey(context.Background(), formation.UID, "tsc_kickoff")
	require.NoError(t, err)
	assert.Equal(t, model.StatusNotStarted, after.Status)
	assert.Equal(t, model.SourcePlatform, after.StatusSource,
		"the row stays marked platform; it is the lookup that is missing, not the intent")
}

// The registry is empty on purpose, and the three template rows depend on that
// being true rather than on it being an oversight. If a lookup is added, this
// fails and whoever added it has to say so here.
func TestTheLookupRegistryIsDeliberatelyEmpty(t *testing.T) {
	if len(platformLookups) != 0 {
		t.Errorf("platformLookups has %d entries. If an owning service now answers a "+
			"project-scoped lookup, update this test and the comment above the registry — "+
			"and check the row is still one the platform should decide.", len(platformLookups))
	}
}

// A lookup that fails leaves the item alone and does not fail the pass: one
// resource type being unreachable must not stop the others being resolved.
func TestAFailingLookupLeavesTheItemAloneAndDoesNotFailThePass(t *testing.T) {
	broken := func(_ context.Context, _ string) (*model.ResolvedRef, error) {
		return nil, errors.New("committee service unreachable")
	}
	checker, repos, formation := platformFixture(t, model.StatusInProgress, broken)

	report, err := checker.ResolveFor(context.Background(), "project-1")
	require.NoError(t, err, "one unreachable service failed the whole pass")
	assert.Equal(t, 1, report.Failed)
	assert.Equal(t, 0, report.Advanced)

	after, err := repos.items.GetByKey(context.Background(), formation.UID, "tsc_kickoff")
	require.NoError(t, err)
	assert.Equal(t, model.StatusInProgress, after.Status)
}

// A completed or frozen checklist is not advanced. Its project has gone Active or
// been archived, and rewriting its rows afterwards edits the record of how it got
// there.
func TestAReadOnlyChecklistIsNotAdvanced(t *testing.T) {
	checker, repos, formation := platformFixture(t, model.StatusInProgress, foundCommittee)

	_, err := repos.formations.UpdateLifecycle(
		context.Background(), formation.UID, model.LifecycleCompleted, formation.Revision)
	require.NoError(t, err)

	report, err := checker.ResolveFor(context.Background(), "project-1")
	require.NoError(t, err)
	assert.Equal(t, 0, report.Advanced)

	after, err := repos.items.GetByKey(context.Background(), formation.UID, "tsc_kickoff")
	require.NoError(t, err)
	assert.Equal(t, model.StatusInProgress, after.Status)
}

// A project with no checklist is not an error. The reconcile may not have created
// one yet.
func TestAProjectWithNoChecklistIsNotAFailure(t *testing.T) {
	checker, _, _ := platformFixture(t, model.StatusNotStarted, foundCommittee)

	report, err := checker.ResolveFor(context.Background(), "no-such-project")
	require.NoError(t, err)
	assert.Equal(t, 0, report.Advanced)
	assert.Equal(t, 0, report.Failed)
}
