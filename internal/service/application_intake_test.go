// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
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
			"project_name":              "Proposed Project",
			"project_repository_url":    "https://github.com/example/proposed",
			"project_website":           "https://proposed.example.test",
			"trademark_status":          "not_sure",
			"contributing_organization": "Example Organization",
			"legal_contact_email":       "legal@example.test",
			"formation_list":            []any{"one@example.test", "two@example.test"},
			"license":                   "Apache-2.0",
			"chat_platform":             "slack",
			"mission_statement":         "Build the proposed project.",
			"agreement_type":            "dco",
			"is_spec_project":           false,
			"description":               "A proposed open source project.",
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

func snapshotApplication(t *testing.T, application map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(application)
	require.NoError(t, err)
	var snapshot map[string]any
	require.NoError(t, json.Unmarshal(encoded, &snapshot))
	return snapshot
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

func TestCreateApplicationPreservesCanonicalAnswers(t *testing.T) {
	s, _, _, _ := intakeService(t)
	p := intakePayload()
	want := snapshotApplication(t, p.Application)

	got, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), p)

	require.NoError(t, err)
	assert.Equal(t, want, got.Application)
}

func TestCreateApplicationAcceptsLegacyFormationListAddress(t *testing.T) {
	s, _, _, _ := intakeService(t)
	p := intakePayload()
	p.Application["formation_list"] = []any{"a@b@c"}

	got, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), p)

	require.NoError(t, err)
	assert.Equal(t, []any{"a@b@c"}, got.Application["formation_list"])
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
		name    string
		mutate  func(*svc.CreateApplicationPayload)
		reason  string
		message string
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
			name: "submitter username contains internal whitespace",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.SubmitterUsername = "a smith"
			},
			reason: "submitter_username_required",
		},
		{
			name: "submitter username contains an internal format character",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.SubmitterUsername = "a\u200bsmith"
			},
			reason: "submitter_username_required",
		},
		{
			name: "submitter username is the FGA wildcard",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.SubmitterUsername = "*"
			},
			reason: "submitter_username_required",
		},
		{
			name: "submitter username names another subject type",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.SubmitterUsername = "team:formation"
			},
			reason: "submitter_username_required",
		},
		{
			name: "submitter username names a userset relation",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.SubmitterUsername = "readers#member"
			},
			reason: "submitter_username_required",
		},
		{
			name: "project name has the wrong type",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["project_name"] = false
			},
			reason:  reasonApplicationFieldInvalid,
			message: "project_name must be a string",
		},
		{
			name: "repository is not an HTTP URL",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["project_repository_url"] = "git@example.test:repo"
			},
			reason: reasonApplicationFieldInvalid,
		},
		{
			name: "repository URL has no host",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["project_repository_url"] = "https:repo"
			},
			reason:  reasonApplicationFieldInvalid,
			message: "project_repository_url must be an http or https URL with a host",
		},
		{
			name: "repository URL has a port but no hostname",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["project_repository_url"] = "https://:443/repo"
			},
			reason: reasonApplicationFieldInvalid,
		},
		{
			name: "website is not a URL",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["project_website"] = "proposed.example.test"
			},
			reason: reasonProjectWebsiteBad,
		},
		{
			name: "trademark status has the wrong type",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["trademark_status"] = true
			},
			reason: reasonApplicationFieldInvalid,
		},
		{
			name: "contributing organization has the wrong type",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["contributing_organization"] = []any{}
			},
			reason: reasonApplicationFieldInvalid,
		},
		{
			name: "legal contact is not an email address",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["legal_contact_email"] = "not-an-address"
			},
			reason:  reasonApplicationFieldInvalid,
			message: "legal_contact_email must be an email address",
		},
		{
			name: "legal contact has multiple at signs",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["legal_contact_email"] = "a@b@c"
			},
			reason: reasonApplicationFieldInvalid,
		},
		{
			name: "legal contact contains Unicode whitespace",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["legal_contact_email"] = "a@\nb"
			},
			reason: reasonApplicationFieldInvalid,
		},
		{
			name: "legal contact has surrounding Unicode whitespace",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["legal_contact_email"] = "\u00a0a@example.test"
			},
			reason: reasonApplicationFieldInvalid,
		},
		{
			name: "legal contact contains a control character",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["legal_contact_email"] = "a@\x07b"
			},
			reason:  reasonApplicationFieldInvalid,
			message: "legal_contact_email must be an email address",
		},
		{
			name: "nested answer contains NUL",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["future_field"] = map[string]any{
					"nested": []any{"a\x00b"},
				}
			},
			reason:  reasonApplicationFieldInvalid,
			message: "application must not contain NUL characters",
		},
		{
			name: "formation list carries something that is not an address",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["formation_list"] = []any{"not-an-address"}
			},
			reason: reasonFormationListInvalid,
		},
		{
			name: "license has the wrong type",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["license"] = 42
			},
			reason: reasonApplicationFieldInvalid,
		},
		{
			name: "chat platform has the wrong type",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["chat_platform"] = false
			},
			reason: reasonApplicationFieldInvalid,
		},
		{
			name: "mission statement has the wrong type",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["mission_statement"] = []any{}
			},
			reason: reasonApplicationFieldInvalid,
		},
		{
			name: "agreement type has the wrong type",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["agreement_type"] = 7
			},
			reason: reasonApplicationFieldInvalid,
		},
		{
			name: "specification flag has the wrong type",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["is_spec_project"] = "yes"
			},
			reason:  reasonApplicationFieldInvalid,
			message: "is_spec_project must be a boolean",
		},
		{
			name: "description has the wrong type",
			mutate: func(p *svc.CreateApplicationPayload) {
				p.Application["description"] = map[string]any{}
			},
			reason: reasonApplicationFieldInvalid,
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
			if tc.message != "" {
				assert.Equal(t, tc.message, appErr.Message)
			}
			assert.NotContains(t, applications.Calls(), "applications.Create",
				"a refused payload must not be stored")
		})
	}
}

func TestCreateApplicationPreservesLegacyValidationPrecedence(t *testing.T) {
	cases := map[string]struct {
		application map[string]any
		reason      string
	}{
		"website before canonical fields": {
			application: map[string]any{
				"project_website": "not-a-url",
				"is_spec_project": "yes",
			},
			reason: reasonProjectWebsiteBad,
		},
		"formation list before canonical fields": {
			application: map[string]any{
				"formation_list":  []any{"not-an-address"},
				"is_spec_project": "yes",
			},
			reason: reasonFormationListInvalid,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, _, _ := intakeService(t)
			p := intakePayload()
			p.Application = tc.application

			_, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), p)

			var appErr *svc.ApplicationError
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, tc.reason, appErr.Reason)
		})
	}
}

func TestCreateApplicationAcceptsMissingNullAndBlankCanonicalFields(t *testing.T) {
	textFields := []string{
		"project_name",
		"project_repository_url",
		"project_website",
		"trademark_status",
		"contributing_organization",
		"legal_contact_email",
		"license",
		"chat_platform",
		"mission_statement",
		"agreement_type",
		"description",
	}
	allFields := append(append([]string{}, textFields...), "formation_list", "is_spec_project")

	cases := map[string]map[string]any{
		"no canonical fields":  {},
		"empty formation list": {"formation_list": []any{}},
		"unknown field and value": {
			"future_field": map[string]any{"nested": []any{"value"}},
		},
		"arbitrary trademark status": {
			"trademark_status": "anything-at-all",
		},
		"arbitrary license": {
			"license": "custom-license-expression",
		},
		"arbitrary chat platform": {
			"chat_platform": "carrier-pigeon",
		},
		"arbitrary agreement type": {
			"agreement_type": "handshake",
		},
		"legacy scheme-only website": {
			"project_website": "https:site",
		},
		"false specification flag": {
			"is_spec_project": false,
		},
		"true specification flag": {
			"is_spec_project": true,
		},
	}
	for _, field := range allFields {
		cases[field+" is null"] = map[string]any{field: nil}
	}
	for _, field := range textFields {
		cases[field+" is blank"] = map[string]any{field: ""}
	}

	for name, application := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, _, _ := intakeService(t)
			p := intakePayload()
			p.Application = application

			got, err := s.CreateApplication(asPrincipal("lfx-ui@clients"), p)

			require.NoError(t, err)
			assert.Equal(t, application, got.Application)
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
