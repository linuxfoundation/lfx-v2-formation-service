// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
	natsinfra "github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/nats"
)

func intakePayload() *svc.CreateApplicationPayload {
	return &svc.CreateApplicationPayload{
		Version:           "1",
		SubmitterUsername: "asmith",
		SubmitterName:     "A Smith",
		SubmitterEmail:    "asmith@example.test",
		Application: map[string]any{
			"project_name":    "Proposed Project",
			"project_website": "https://proposed.example.test",
			"formation_list":  []any{"one@example.test", "two@example.test"},
		},
	}
}

// intakeService wires the intake route over doubles, returning the whole set
// so a test can inspect what the create did and did not touch.
func intakeService(t *testing.T) (
	*Service,
	*mock.ApplicationRepository,
	*mock.FormationRepository,
	*mock.ItemRepository,
) {
	t.Helper()
	applications := mock.NewApplicationRepository()
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	s := NewService(
		WithApplications(applications),
		WithFormations(formations),
		WithItems(items),
	)
	return s, applications, formations, items
}

func TestCreateApplicationRecordsSubmitterFromThePayload(t *testing.T) {
	s, _, _, _ := intakeService(t)

	// The principal is the calling service, not the applicant: this route is
	// reached by the UI authenticating as itself, so the credential and the
	// submitter are different identities by design.
	got, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), intakePayload())

	require.NoError(t, err)
	assert.Equal(t, "asmith", got.SubmitterUsername)
	assert.Equal(t, "asmith@example.test", got.SubmitterEmail)
	assert.NotEmpty(t, got.UID)
	assert.Equal(t, "submitted", got.State)
	assert.Equal(t, int64(1), got.Revision)
	// No parent was sent, and none may be invented. An application with no
	// parent is its ordinary state while under review.
	assert.Nil(t, got.TargetParentUID)
}

func TestCreateApplicationNormalizesSubmitterUsername(t *testing.T) {
	s, _, _, _ := intakeService(t)
	p := intakePayload()
	p.SubmitterUsername = "  asmith\t"

	got, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), p)

	require.NoError(t, err)
	assert.Equal(t, "asmith", got.SubmitterUsername)
}

// A submission produces an application and no other platform state.
// The doubles are inspected directly rather than through the API, because the
// failure this guards against is a create that quietly does something extra.
func TestCreateApplicationCreatesNothingElse(t *testing.T) {
	s, _, formations, items := intakeService(t)

	_, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), intakePayload())
	require.NoError(t, err)

	assert.Empty(t, formations.Calls(), "creating an application must not touch the formation store")
	assert.Empty(t, items.Calls(), "creating an application must not touch the item store")
}

// The target parent is carried through and is never read as a placement. The
// assertion that matters is the second one: nothing about the stored record
// treats it as where the project will sit.
func TestCreateApplicationCarriesTargetParentAsAHint(t *testing.T) {
	s, _, formations, _ := intakeService(t)

	parent := "7f1c3b6e-0000-4000-8000-000000000001"
	p := intakePayload()
	p.TargetParentUID = &parent

	got, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), p)

	require.NoError(t, err)
	require.NotNil(t, got.TargetParentUID)
	assert.Equal(t, parent, *got.TargetParentUID)
	assert.Empty(t, formations.Calls(), "a target parent must not reach the formation store")
}

func TestCreateApplicationRefusesInvalidPayloads(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*svc.CreateApplicationPayload)
		reason string
	}{
		{
			name: "submitter username is blank",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.SubmitterUsername = " \t "
			},
			reason: "submitter_username_required",
		},
		{
			name: "submitter username contains only format characters",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.SubmitterUsername = "\u200b\ufeff"
			},
			reason: "submitter_username_required",
		},
		{
			name: "website is not a URL",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["project_website"] = "proposed.example.test"
			},
			reason: reasonProjectWebsiteBad,
		},
		{
			name: "formation list carries something that is not an address",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["formation_list"] = []any{"not-an-address"}
			},
			reason: reasonFormationListInvalid,
		},
		{
			name: "project name would overfill the duplicated index metadata",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["project_name"] = strings.Repeat("x", maxProjectNameBytes+1)
			},
			reason: reasonApplicationTooLarge,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, applications, _, _ := intakeService(t)
			p := intakePayload()
			tc.mutate(p)

			_, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), p)

			var appErr *svc.ApplicationError
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, tc.reason, appErr.Reason)
			assert.Equal(t, "400", appErr.Code)
			assert.NotContains(t, applications.Calls(), "applications.Create",
				"a refused payload must not be stored")
		})
	}
}

func TestCreateApplicationRefusesAnswersAboveThePublishLimit(t *testing.T) {
	s, applications, _, _ := intakeService(t)
	p := intakePayload()
	p.Application["description"] = strings.Repeat("x", maxApplicationPayloadBytes)

	_, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), p)

	var appErr *svc.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, reasonApplicationTooLarge, appErr.Reason)
	assert.NotContains(t, applications.Calls(), "applications.Create")
}

func TestCreateApplicationRefusesAnOversizedProjectionBeforeStoring(t *testing.T) {
	applications := mock.NewApplicationRepository()
	s := NewService(
		WithApplications(applications),
		WithApplicationProjectionValidator(natsinfra.NewApplicationProjectionValidator()),
	)
	p := intakePayload()
	p.SubmitterUsername = strings.Repeat("u", 256<<10)
	p.Application["description"] = strings.Repeat("d", 400<<10)
	p.Application["project_name"] = strings.Repeat("p", maxProjectNameBytes)

	_, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), p)

	var appErr *svc.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, reasonApplicationTooLarge, appErr.Reason)
	assert.NotContains(t, applications.Calls(), "applications.Create")
}

// People named for the formation work are stored as addresses and nothing
// more. Resolving them to platform identities, granting them anything or
// notifying them are all things this route must not do, and the observable
// consequence is that the addresses come back exactly as sent.
func TestCreateApplicationStoresFormationListAsAddressesOnly(t *testing.T) {
	s, _, _, _ := intakeService(t)

	got, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), intakePayload())

	require.NoError(t, err)
	assert.Equal(t,
		[]any{"one@example.test", "two@example.test"},
		got.Application["formation_list"])
}

// An unwired store is a deployment fault, not a bad request: the caller must
// not be told their payload was invalid when it was fine.
func TestCreateApplicationWithNoStoreFailsClosed(t *testing.T) {
	s := NewService()

	_, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), intakePayload())

	require.Error(t, err)
	var appErr *svc.ApplicationError
	assert.False(t, errors.As(err, &appErr), "an unwired store must not surface as a declared refusal")
}
