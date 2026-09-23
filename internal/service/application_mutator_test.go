// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
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
	assert.Equal(t, created.Revision, projected.Revision)
	assert.Equal(t, payload.Application, projected.Payload)
	require.NotNil(t, projected.TargetParentUID)
	assert.Equal(t, target, *projected.TargetParentUID)
}

// A publish failure does not undo the record.
//
// The application is already committed by the time the publish is attempted,
// and telling the caller their submission failed would get it filed twice.
// What they lose is visibility until something republishes.
func TestCreateApplicationSurvivesPublishFailures(t *testing.T) {
	for name, fail := range applicationPublisherFailures() {
		t.Run(name, func(t *testing.T) {
			d := applicationService(t)
			fail(d)

			created, err := d.service.CreateApplication(asPrincipal("lfx-ui@clients"), intakePayload())

			require.NoError(t, err, "a publish failure must not be reported as a rejected submission")
			require.NotNil(t, created)
			stored, err := d.applications.Get(context.Background(), mustUUID(t, created.UID))
			require.NoError(t, err, "the record must survive the failed publish")
			assert.Equal(t, model.ApplicationSubmitted, stored.State)
		})
	}
}

func TestReviseApplicationReplacesTheAnswers(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	revised, err := d.service.ReviseApplication(asPrincipal("asmith"), &svc.ReviseApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
		Application: map[string]any{
			"project_name": "Renamed Project",
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "Renamed Project", revised.Application.Application["project_name"])
	assert.Equal(t, created.Revision+1, revised.Application.Revision)
	require.NotNil(t, revised.Etag)
	assert.Equal(t, "2", *revised.Etag)
	// A replacement, not a merge: the website the original submission carried
	// is gone because the revision did not carry it.
	assert.NotContains(t, revised.Application.Application, "project_website")

	stored, err := d.applications.Get(context.Background(), mustUUID(t, created.UID))
	require.NoError(t, err)
	assert.Equal(t, "Renamed Project", stored.Payload["project_name"])
	assert.Equal(t, model.ApplicationSubmitted, stored.State,
		"revising does not move the state")
}

func TestApplicationMutationETagWorksAsTheNextIfMatch(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	revised, err := d.service.ReviseApplication(asPrincipal("asmith"), &svc.ReviseApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
		Application: map[string]any{"project_name": "Renamed"},
	})
	require.NoError(t, err)
	require.NotNil(t, revised.Etag)
	nextRevision, err := strconv.ParseInt(*revised.Etag, 10, 64)
	require.NoError(t, err)

	withdrawn, err := d.service.WithdrawApplication(
		asPrincipal("asmith"),
		&svc.WithdrawApplicationPayload{
			Version: "1", UID: created.UID, IfMatch: nextRevision,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, nextRevision+1, withdrawn.Application.Revision)
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
				Version: "1", UID: created.UID, IfMatch: created.Revision, Application: answers,
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
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})

	require.NoError(t, err)
	assert.Equal(t, string(model.ApplicationWithdrawn), withdrawn.Application.State)

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
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})
	require.NoError(t, err)

	revised, err := d.service.ReviseApplication(asPrincipal("asmith"), &svc.ReviseApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision + 1,
		Application: map[string]any{"project_name": "Revised after decision"},
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.ApplicationDenied), revised.Application.State)

	withdrawn, err := d.service.WithdrawApplication(asPrincipal("asmith"), &svc.WithdrawApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision + 2,
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.ApplicationWithdrawn), withdrawn.Application.State)
}

func TestMutationsRefuseAnIdentifierThatIsNotAUUID(t *testing.T) {
	d := applicationService(t)

	_, err := d.service.WithdrawApplication(asPrincipal("asmith"), &svc.WithdrawApplicationPayload{
		Version: "1", UID: "not-a-uuid", IfMatch: 1,
	})

	var refusal *svc.ApplicationError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "400", refusal.Code)
}

func TestApplicationMutationsSurvivePublishFailures(t *testing.T) {
	operations := applicationMutationCases()

	for _, operation := range operations {
		for publisher, fail := range applicationPublisherFailures() {
			t.Run(operation.name+"/"+publisher, func(t *testing.T) {
				d := applicationService(t)
				created := submitOne(t, d.service)
				fail(d)

				require.NoError(t, operation.act(d.service, created.UID))
				operation.assertCommitted(t, d, created.UID)
			})
		}
	}
}

func applicationPublisherFailures() map[string]func(applicationDoubles) {
	return map[string]func(applicationDoubles){
		"access":  func(d applicationDoubles) { d.access.Err = assert.AnError },
		"indexer": func(d applicationDoubles) { d.indexer.SetError(assert.AnError) },
	}
}

func TestApplicationMutationsFailClosedWithoutAUnitOfWork(t *testing.T) {
	s := NewService(WithApplications(mock.NewApplicationRepository()))
	uid := uuid.NewString()

	for _, operation := range applicationMutationCases() {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.act(s, uid)
			require.Error(t, err)
			var refusal *svc.ApplicationError
			assert.False(t, errors.As(err, &refusal))
		})
	}
}

func TestApplicationMutationsRejectAStaleRevision(t *testing.T) {
	cases := map[string]func(*Service, string) error{
		"revise": func(s *Service, uid string) error {
			_, err := s.ReviseApplication(asPrincipal("asmith"), &svc.ReviseApplicationPayload{
				Version: "1", UID: uid, IfMatch: 2,
				Application: map[string]any{"project_name": "Stale"},
			})
			return err
		},
		"withdraw": func(s *Service, uid string) error {
			_, err := s.WithdrawApplication(asPrincipal("asmith"),
				&svc.WithdrawApplicationPayload{Version: "1", UID: uid, IfMatch: 2})
			return err
		},
		"accept": func(s *Service, uid string) error {
			_, err := s.AcceptApplication(asPrincipal("staff"),
				&svc.AcceptApplicationPayload{Version: "1", UID: uid, IfMatch: 2})
			return err
		},
		"deny": func(s *Service, uid string) error {
			_, err := s.DenyApplication(asPrincipal("staff"),
				&svc.DenyApplicationPayload{Version: "1", UID: uid, IfMatch: 2})
			return err
		},
		"delete": func(s *Service, uid string) error {
			return s.DeleteApplication(asPrincipal("staff"),
				&svc.DeleteApplicationPayload{Version: "1", UID: uid, IfMatch: 2})
		},
	}

	for name, act := range cases {
		t.Run(name, func(t *testing.T) {
			d := applicationService(t)
			created := submitOne(t, d.service)

			err := act(d.service, created.UID)

			var refusal *svc.ApplicationError
			require.ErrorAs(t, err, &refusal)
			assert.Equal(t, "412", refusal.Code)
			assert.Equal(t, reasonVersionMismatch, refusal.Reason)
			stored, getErr := d.applications.Get(context.Background(), mustUUID(t, created.UID))
			require.NoError(t, getErr)
			assert.Equal(t, created.Revision, stored.Revision)
			assert.Equal(t, model.ApplicationSubmitted, stored.State)
			assert.Empty(t, d.access.Deleted())
			assert.Empty(t, d.indexer.ApplicationDeleted())
		})
	}
}

func TestStaleMutationRepublishesTheCurrentRevision(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)
	uid := mustUUID(t, created.UID)
	current, err := d.applications.UpdatePayload(
		context.Background(), uid, created.Revision,
		map[string]any{"project_name": "Current answers"},
	)
	require.NoError(t, err)
	assert.Equal(t, created.Revision, d.indexer.LatestApplication(created.UID).Revision,
		"the setup requires a stale indexed projection")

	_, err = d.service.AcceptApplication(asPrincipal("staff"), &svc.AcceptApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})

	var refusal *svc.ApplicationError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, reasonVersionMismatch, refusal.Reason)
	projected := d.indexer.LatestApplication(created.UID)
	require.NotNil(t, projected)
	assert.Equal(t, current.Revision, projected.Revision)
	assert.Equal(t, "Current answers", projected.Payload["project_name"])
}

type applicationMutationCase struct {
	name            string
	act             func(*Service, string) error
	assertCommitted func(*testing.T, applicationDoubles, string)
}

func applicationMutationCases() []applicationMutationCase {
	stateIs := func(want model.ApplicationState) func(*testing.T, applicationDoubles, string) {
		return func(t *testing.T, d applicationDoubles, uid string) {
			stored, err := d.applications.Get(context.Background(), mustUUID(t, uid))
			require.NoError(t, err)
			assert.Equal(t, want, stored.State)
		}
	}
	payloadProjectNameIs := func(want string) func(*testing.T, applicationDoubles, string) {
		return func(t *testing.T, d applicationDoubles, uid string) {
			stored, err := d.applications.Get(context.Background(), mustUUID(t, uid))
			require.NoError(t, err)
			assert.Equal(t, model.ApplicationSubmitted, stored.State)
			assert.Equal(t, want, stored.Payload["project_name"])
		}
	}
	return []applicationMutationCase{
		{"revise", reviseApplication, payloadProjectNameIs("Revised")},
		{"withdraw", withdrawApplication, stateIs(model.ApplicationWithdrawn)},
		{"accept", acceptApplication, stateIs(model.ApplicationAccepted)},
		{"deny", denyApplication, stateIs(model.ApplicationDenied)},
		{"delete", deleteApplication, assertApplicationDeleted},
	}
}

func reviseApplication(s *Service, uid string) error {
	_, err := s.ReviseApplication(asPrincipal("asmith"), &svc.ReviseApplicationPayload{
		Version: "1", UID: uid, IfMatch: 1, Application: map[string]any{"project_name": "Revised"},
	})
	return err
}

func withdrawApplication(s *Service, uid string) error {
	_, err := s.WithdrawApplication(asPrincipal("asmith"),
		&svc.WithdrawApplicationPayload{Version: "1", UID: uid, IfMatch: 1})
	return err
}

func acceptApplication(s *Service, uid string) error {
	_, err := s.AcceptApplication(asPrincipal("staff"),
		&svc.AcceptApplicationPayload{Version: "1", UID: uid, IfMatch: 1})
	return err
}

func denyApplication(s *Service, uid string) error {
	_, err := s.DenyApplication(asPrincipal("staff"),
		&svc.DenyApplicationPayload{Version: "1", UID: uid, IfMatch: 1})
	return err
}

func deleteApplication(s *Service, uid string) error {
	return s.DeleteApplication(asPrincipal("staff"),
		&svc.DeleteApplicationPayload{Version: "1", UID: uid, IfMatch: 1})
}

func assertApplicationDeleted(t *testing.T, d applicationDoubles, uid string) {
	_, err := d.applications.Get(context.Background(), mustUUID(t, uid))
	assert.ErrorIs(t, err, domain.ErrNotFound)
}
