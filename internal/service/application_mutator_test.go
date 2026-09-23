// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// applicationDoubles is the full application surface wired over doubles —
// store, unit of work and both publishers — so a test can assert on what a
// write published as well as on what it stored.
type applicationDoubles struct {
	service      *Service
	applications *mock.ApplicationRepository
	access       *mock.AccessPublisher
	indexer      *mock.IndexerPublisher
}

// Returned as a struct rather than as four values because each test reads
// only one or two of them, and positional blanks stop saying which double was
// skipped once there are more than about three.
func applicationService(t *testing.T) applicationDoubles {
	t.Helper()
	applications := mock.NewApplicationRepository()
	access := mock.NewAccessPublisher()
	indexer := mock.NewIndexerPublisher()
	uow := mock.NewUnitOfWork(
		mock.NewFormationRepository(),
		mock.NewItemRepository(),
		mock.NewActivityRepository(),
		mock.NewTemplateRepository(),
		applications,
	)
	return applicationDoubles{
		service: NewService(
			WithApplications(applications),
			WithUnitOfWork(uow),
			WithApplicationAccess(access),
			WithApplicationIndexer(indexer),
			WithApplicationTeam("formation"),
		),
		applications: applications,
		access:       access,
		indexer:      indexer,
	}
}

func mustUUID(t *testing.T, raw string) uuid.UUID {
	t.Helper()
	parsed, err := uuid.Parse(raw)
	require.NoError(t, err)
	return parsed
}

func submitOne(t *testing.T, s *Service) *svc.ProjectApplication {
	t.Helper()
	created, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), intakePayload())
	require.NoError(t, err)
	return created
}

// Creating an application grants the submitter their standing on it.
//
// Without this tuple the applicant cannot read the application they just
// filed: the query service resolves a per-document check, and the submitter
// relation is the only thing that makes it pass for them.
func TestCreateApplicationGrantsTheSubmitter(t *testing.T) {
	d := applicationService(t)

	created := submitOne(t, d.service)

	published := d.access.Published()
	require.Len(t, published, 1, "creating an application must grant access exactly once")
	assert.Equal(t, created.UID, published[0].ApplicationUID)
	assert.Equal(t, "asmith", published[0].SubmitterUsername,
		"the grant names the applicant, not the calling service that authenticated")
	assert.Equal(t, "formation", published[0].FormationTeam)
	projected := d.indexer.LatestApplication(created.UID)
	require.NotNil(t, projected)
	assert.Equal(t, "viewer", projected.AccessRelation)
}

func TestCreateApplicationIndexesReviewDetails(t *testing.T) {
	d := applicationService(t)
	payload := intakePayload()
	target := "7f1c3b6e-0000-4000-8000-000000000001"
	payload.TargetParentUID = &target

	created, err := d.service.CreateApplication(asPrincipal("lfx-ui@clients"), payload)
	require.NoError(t, err)

	projected := d.indexer.LatestApplication(created.UID)
	require.NotNil(t, projected)
	assert.Equal(t, payload.Application, projected.Payload)
	require.NotNil(t, projected.TargetParentUID)
	assert.Equal(t, target, *projected.TargetParentUID)
}

// A failed grant does not undo the record.
//
// The application is already committed by the time the publish is attempted,
// and telling the caller their submission failed would get it filed twice.
// What they lose is visibility until something republishes.
func TestCreateApplicationSurvivesAFailedGrant(t *testing.T) {
	d := applicationService(t)
	d.access.Err = assert.AnError

	created, err := d.service.CreateApplication(asPrincipal("lfx-ui@clients"), intakePayload())

	require.NoError(t, err, "a publish failure must not be reported as a rejected submission")
	require.NotNil(t, created)
	stored, err := d.applications.Get(context.Background(), mustUUID(t, created.UID))
	require.NoError(t, err, "the record must survive the failed grant")
	assert.Equal(t, model.ApplicationSubmitted, stored.State)
}

func TestReviseApplicationReplacesTheAnswers(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	revised, err := d.service.ReviseApplication(asPrincipal("asmith"), &svc.ReviseApplicationPayload{
		Version: "1",
		UID:     created.UID,
		Application: map[string]any{
			"project_name": "Renamed Project",
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "Renamed Project", revised.Application["project_name"])
	// A replacement, not a merge: the website the original submission carried
	// is gone because the revision did not carry it.
	assert.NotContains(t, revised.Application, "project_website")

	stored, err := d.applications.Get(context.Background(), mustUUID(t, created.UID))
	require.NoError(t, err)
	assert.Equal(t, "Renamed Project", stored.Payload["project_name"])
	assert.Equal(t, model.ApplicationSubmitted, stored.State,
		"revising does not move the state")
}

// A revision is held to the same rules as the original submission. Otherwise
// a payload refused at intake gets in through the next edit.
func TestReviseApplicationValidatesLikeIntake(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	cases := map[string]map[string]any{
		"a bad website":        {"project_name": "P", "project_website": "javascript:alert(1)"},
		"a bad formation list": {"project_name": "P", "formation_list": []any{"not-an-address"}},
	}

	for name, answers := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := d.service.ReviseApplication(asPrincipal("asmith"), &svc.ReviseApplicationPayload{
				Version: "1", UID: created.UID, Application: answers,
			})
			require.Error(t, err)
			var refusal *svc.ApplicationError
			require.ErrorAs(t, err, &refusal)
			assert.Equal(t, "400", refusal.Code)
		})
	}
}

func TestWithdrawApplicationKeepsTheRecord(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	withdrawn, err := d.service.WithdrawApplication(asPrincipal("asmith"), &svc.WithdrawApplicationPayload{
		Version: "1", UID: created.UID,
	})

	require.NoError(t, err)
	assert.Equal(t, string(model.ApplicationWithdrawn), withdrawn.State)

	// Withdrawing is not deleting. The row survives, which is the only thing
	// distinguishing a withdrawn application from one that was never filed.
	stored, err := d.applications.Get(context.Background(), mustUUID(t, created.UID))
	require.NoError(t, err)
	assert.Equal(t, model.ApplicationWithdrawn, stored.State)
}

func TestReviseAndWithdrawAreNotStateGated(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)
	_, err := d.service.DenyApplication(asPrincipal("reviewer-one"), &svc.DenyApplicationPayload{
		Version: "1", UID: created.UID,
	})
	require.NoError(t, err)

	revised, err := d.service.ReviseApplication(asPrincipal("asmith"), &svc.ReviseApplicationPayload{
		Version: "1", UID: created.UID,
		Application: map[string]any{"project_name": "Revised after decision"},
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.ApplicationDenied), revised.State)

	withdrawn, err := d.service.WithdrawApplication(asPrincipal("asmith"), &svc.WithdrawApplicationPayload{
		Version: "1", UID: created.UID,
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.ApplicationWithdrawn), withdrawn.State)
}

func TestMutationsRefuseAnIdentifierThatIsNotAUUID(t *testing.T) {
	d := applicationService(t)

	_, err := d.service.WithdrawApplication(asPrincipal("asmith"), &svc.WithdrawApplicationPayload{
		Version: "1", UID: "not-a-uuid",
	})

	var refusal *svc.ApplicationError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "400", refusal.Code)
}
