// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/log"
)

// Refusals cover an unusable access subject and the application constraints
// this service validates.
const (
	reasonSubmitterUsernameRequired = "submitter_username_required"
	reasonProjectWebsiteBad         = "project_website_invalid"
	reasonFormationListInvalid      = "formation_list_invalid"
	reasonApplicationFieldInvalid   = "application_field_invalid"
	reasonApplicationTooLarge       = "application_payload_too_large"
)

var applicationReasonMessages = map[string]string{
	reasonNotFound:                  "no such application",
	reasonVersionMismatch:           "if-match did not match the application's current revision",
	reasonSubmitterUsernameRequired: "submitter_username is required and must name one user",
	reasonProjectWebsiteBad:         "project_website must be an http or https URL",
	reasonFormationListInvalid:      "formation_list must be a list of email addresses",
	reasonApplicationFieldInvalid:   "an application field has an invalid value",
	reasonApplicationTooLarge:       "application submission exceeds the transport-safe size limit",
	reasonApplicationUIDBad:         "the application identifier is not a uuid",
}

// The answer limit leaves room below the index envelope cap for submitter
// fields and JSON overhead.
const (
	maxApplicationPayloadBytes = 512 << 10
	maxProjectNameBytes        = 64 << 10
)

// Intake payload keys this service knows about by name. Everything else in
// the map is carried through untouched.
const (
	payloadProjectName              = "project_name"
	payloadProjectRepositoryURL     = "project_repository_url"
	payloadProjectWebsite           = "project_website"
	payloadTrademarkStatus          = "trademark_status"
	payloadContributingOrganization = "contributing_organization"
	payloadLegalContactEmail        = "legal_contact_email"
	payloadFormationList            = "formation_list"
	payloadLicense                  = "license"
	payloadChatPlatform             = "chat_platform"
	payloadMissionStatement         = "mission_statement"
	payloadAgreementType            = "agreement_type"
	payloadIsSpecProject            = "is_spec_project"
	payloadDescription              = "description"
)

// CreateApplication records a proposal to start a new foundation.
//
// It creates an application and nothing else: no project, no checklist, no
// stage assignment, no entity placement. That is the whole point of the object
// — an application with no parent and no incorporated entity is its ordinary
// state under review, and none of those questions has an answer yet.
//
// The submitter is recorded from the payload rather than from the credential.
// The UI calls this authenticating as itself, so no end-user token reaches
// this service and nothing here attests who the applicant is. Applications do
// not store the calling service as an actor.
func (s *Service) CreateApplication(
	ctx context.Context, p *svc.CreateApplicationPayload,
) (*svc.ProjectApplication, error) {
	if s.applications == nil {
		// Fails closed as a 500 rather than as a declared refusal: an
		// unwired store is a deployment fault, and telling the caller their
		// submission was invalid would be a lie they might act on.
		slog.ErrorContext(ctx, "formationService.create-application: no application repository wired")
		return nil, errors.New("application storage is not available")
	}

	application, err := validateIntake(p)
	if err != nil {
		return nil, mapApplicationError(err)
	}

	now := time.Now().UTC()
	pending := &model.Application{
		UID:               uuid.New(),
		State:             model.ApplicationSubmitted,
		Revision:          1,
		SubmitterUsername: p.SubmitterUsername,
		SubmitterName:     p.SubmitterName,
		SubmitterEmail:    p.SubmitterEmail,
		TargetParentUID:   p.TargetParentUID,
		Payload:           application,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := s.validateApplicationProjection(pending); err != nil {
		return nil, mapApplicationError(err)
	}

	created, err := s.applications.Create(ctx, pending)
	if err != nil {
		slog.ErrorContext(ctx, "formationService.create-application", log.ErrKey, err)
		return nil, err
	}

	slog.InfoContext(ctx, "formationService.create-application",
		"application_uid", created.UID,
		"has_target_parent", created.TargetParentUID != nil,
	)

	_ = s.publishApplication(ctx, created)

	return applicationToWire(created), nil
}

// publishApplication grants access on an application and projects it into the
// search index, in that order.
//
// Order matters and is not incidental: the query service resolves a
// per-document access check before returning anything, so a document indexed
// before its grants exist is invisible until the next write republishes it.
// Granting first narrows that window to nothing.
//
// Neither failure changes the API response. PostgreSQL is the source of truth,
// and the reconcile sweep republishes both grants and documents. Returning a
// publish failure after a database write can make a caller repeat a successful
// mutation.
func (s *Service) publishApplication(ctx context.Context, a *model.Application) error {
	var publishErrs []error
	if s.applicationAccess == nil || s.applicationTeam == "" {
		err := errors.New("application access publisher or team is not configured")
		publishErrs = append(publishErrs, err)
		slog.ErrorContext(ctx, "formationService.publish-application: no access publisher wired; the application is readable by nobody",
			"application_uid", a.UID,
		)
	} else if err := s.applicationAccess.PublishApplicationAccess(ctx, port.ApplicationAccess{
		ApplicationUID:    a.UID.String(),
		SubmitterUsername: a.SubmitterUsername,
		FormationTeam:     s.applicationTeam,
	}); err != nil {
		publishErrs = append(publishErrs, err)
		slog.ErrorContext(ctx, "formationService.publish-application: granting access failed",
			"application_uid", a.UID, log.ErrKey, err,
		)
	}

	if s.applicationIndexer == nil {
		publishErrs = append(publishErrs, errors.New("application indexer is not configured"))
		return errors.Join(publishErrs...)
	}
	if err := s.applicationIndexer.PublishApplication(ctx, applicationProjection(a)); err != nil {
		publishErrs = append(publishErrs, err)
		slog.ErrorContext(ctx, "formationService.publish-application: indexing failed",
			"application_uid", a.UID, log.ErrKey, err,
		)
	}
	return errors.Join(publishErrs...)
}

// applicationProjection builds the searchable view of an application.
//
// The private projection carries the complete answers and target-parent hint
// so query-service reads can drive review and prefill project creation.
func applicationProjection(a *model.Application) *port.ApplicationProjection {
	projectName, _ := a.Payload[payloadProjectName].(string)
	return &port.ApplicationProjection{
		ApplicationUID:    a.UID.String(),
		State:             string(a.State),
		Revision:          a.Revision,
		SubmitterUsername: a.SubmitterUsername,
		SubmitterName:     a.SubmitterName,
		SubmitterEmail:    a.SubmitterEmail,
		ProjectName:       projectName,
		Payload:           a.Payload,
		TargetParentUID:   a.TargetParentUID,
		CreatedAt:         a.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:         a.UpdatedAt.UTC().Format(time.RFC3339),
		AccessRelation:    constants.RelationApplicationViewer,
	}
}

func (s *Service) validateApplicationProjection(a *model.Application) error {
	if s.applicationProjectionValidator == nil {
		return nil
	}
	if err := s.applicationProjectionValidator.ValidateApplication(applicationProjection(a)); err != nil {
		return domain.NewReasonError(domain.ErrInvalidRequest, reasonApplicationTooLarge)
	}
	return nil
}

// validateIntake checks the submitter envelope and questionnaire answers,
// including the approved canonical field shapes, and returns the answers to
// store. Unknown answer fields are carried through untouched.
func validateIntake(p *svc.CreateApplicationPayload) (map[string]any, error) {
	p.SubmitterUsername = strings.TrimSpace(p.SubmitterUsername)
	// fga-sync prefixes this value with `user:` verbatim, so FGA subject
	// syntax here would name something other than one user.
	if p.SubmitterUsername == "" ||
		strings.ContainsAny(p.SubmitterUsername, "*:#") ||
		strings.IndexFunc(p.SubmitterUsername, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r)
		}) != -1 {
		return nil, domain.NewReasonError(domain.ErrInvalidRequest, reasonSubmitterUsernameRequired)
	}
	return validateAnswers(p.Application)
}

// validateAnswers checks the questionnaire half of a submission.
//
// Split out from the submitter check because revising reaches it too, and the
// answers a revision may store have to be exactly the answers a submission
// may store. Two copies of this would let a payload be refused at intake and
// accepted on the next edit, which is the same record ending up in a shape
// the create route would never have allowed.
func validateAnswers(answers map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(answers)
	if err != nil || len(encoded) > maxApplicationPayloadBytes {
		return nil, domain.NewReasonError(domain.ErrInvalidRequest, reasonApplicationTooLarge)
	}
	if err := validateCanonicalApplicationFields(answers); err != nil {
		return nil, err
	}
	if projectName, ok := answers[payloadProjectName].(string); ok &&
		len(projectName) > maxProjectNameBytes {
		return nil, domain.NewReasonError(domain.ErrInvalidRequest, reasonApplicationTooLarge)
	}

	// Website keeps its established field-specific refusal; other canonical
	// field failures share reasonApplicationFieldInvalid.
	if err := validateOptionalURL(
		answers, payloadProjectWebsite, reasonProjectWebsiteBad, isSafeURL,
	); err != nil {
		return nil, err
	}

	if err := validateFormationList(answers[payloadFormationList]); err != nil {
		return nil, err
	}

	return answers, nil
}

func validateCanonicalApplicationFields(answers map[string]any) error {
	for _, key := range []string{
		payloadProjectName,
		payloadTrademarkStatus,
		payloadContributingOrganization,
		payloadLicense,
		payloadChatPlatform,
		payloadMissionStatement,
		payloadAgreementType,
		payloadDescription,
	} {
		if err := validateOptionalString(answers, key); err != nil {
			return err
		}
	}
	if err := validateOptionalURL(
		answers, payloadProjectRepositoryURL, reasonApplicationFieldInvalid, isSafeAbsoluteURL,
	); err != nil {
		return err
	}
	if err := validateOptionalEmail(answers, payloadLegalContactEmail); err != nil {
		return err
	}
	return validateOptionalBool(answers, payloadIsSpecProject)
}

func validateOptionalString(answers map[string]any, key string) error {
	value, present := answers[key]
	if !present || value == nil {
		return nil
	}
	if _, ok := value.(string); !ok {
		return domain.NewReasonError(domain.ErrInvalidRequest, reasonApplicationFieldInvalid)
	}
	return nil
}

func validateOptionalURL(
	answers map[string]any, key, reason string, valid func(string) bool,
) error {
	value, present := answers[key]
	if !present || value == nil {
		return nil
	}
	url, ok := value.(string)
	if !ok || (strings.TrimSpace(url) != "" && !valid(strings.TrimSpace(url))) {
		return domain.NewReasonError(domain.ErrInvalidRequest, reason)
	}
	return nil
}

func validateOptionalEmail(answers map[string]any, key string) error {
	value, present := answers[key]
	if !present || value == nil {
		return nil
	}
	address, ok := value.(string)
	if !ok || (strings.TrimSpace(address) != "" && !isEmailAddress(address)) {
		return domain.NewReasonError(domain.ErrInvalidRequest, reasonApplicationFieldInvalid)
	}
	return nil
}

func validateOptionalBool(answers map[string]any, key string) error {
	value, present := answers[key]
	if !present || value == nil {
		return nil
	}
	if _, ok := value.(bool); !ok {
		return domain.NewReasonError(domain.ErrInvalidRequest, reasonApplicationFieldInvalid)
	}
	return nil
}

func isEmailAddress(value string) bool {
	at := strings.Index(value, "@")
	return strings.Count(value, "@") == 1 &&
		at > 0 &&
		at < len(value)-1 &&
		strings.IndexFunc(value, unicode.IsSpace) == -1
}

// validateFormationList checks the legacy address shape for people named for
// the formation work.
//
// Addresses and nothing else. They are never resolved to a platform identity,
// never granted anything and never notified at submit time — the work they
// would be named on does not exist until a project does. Checking the shape
// here keeps them usable later without implying any of that has happened.
func validateFormationList(raw any) error {
	if raw == nil {
		return nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return domain.NewReasonError(domain.ErrInvalidRequest, reasonFormationListInvalid)
	}
	for _, entry := range entries {
		address, ok := entry.(string)
		if !ok {
			return domain.NewReasonError(domain.ErrInvalidRequest, reasonFormationListInvalid)
		}
		address = strings.TrimSpace(address)
		// Preserve the original shallow rule: use the first at-sign only.
		// Existing payloads with another at-sign after it remain accepted.
		at := strings.Index(address, "@")
		if at <= 0 || at == len(address)-1 || strings.Contains(address, " ") {
			return domain.NewReasonErrorf(domain.ErrInvalidRequest, reasonFormationListInvalid,
				"%q is not an email address", address)
		}
	}
	return nil
}

// applicationToWire projects the stored application onto the response shape.
//
// target_parent_uid travels back exactly as it arrived, and carries no more
// meaning going out than it did coming in: it prefills the approver's form and
// decides nothing. No consumer may read it as the application's placement,
// because the entity question is not asked at intake and has no answer yet.
func applicationToWire(a *model.Application) *svc.ProjectApplication {
	return &svc.ProjectApplication{
		UID:               a.UID.String(),
		State:             string(a.State),
		Revision:          a.Revision,
		SubmitterUsername: a.SubmitterUsername,
		SubmitterName:     a.SubmitterName,
		SubmitterEmail:    a.SubmitterEmail,
		TargetParentUID:   a.TargetParentUID,
		Application:       a.Payload,
		CreatedAt:         a.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:         a.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// withApplicationReason gives a bare repository error a reason, so
// mapApplicationError can place it.
//
// The repositories return the sentinel alone, which mapApplicationError
// passes through as a 500 by design — an unrecognised error is a fault, not a
// refusal. That default is right, and it makes this wrapping necessary rather
// than optional: a missing application reaching the mapper unwrapped is
// answered "something went wrong" when the truthful answer is "no such
// application", and the caller retries a request that can never succeed.
//
// Anything other than a not-found is left alone, and so stays a 500. This
// deliberately does not grow into a table mapping every sentinel: a refusal
// the routes did not anticipate is a fault until somebody decides what it
// means.
func withApplicationReason(err error) error {
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrVersionMismatch) {
		var re *domain.ReasonError
		if !errors.As(err, &re) {
			if errors.Is(err, domain.ErrVersionMismatch) {
				return domain.NewReasonError(domain.ErrVersionMismatch, reasonVersionMismatch)
			}
			return domain.NewReasonError(domain.ErrNotFound, reasonNotFound)
		}
	}
	return err
}

// mapApplicationError turns a domain refusal into the declared
// ApplicationError, so Goa's encoder picks the right status. Anything that is
// not a ReasonError falls through as a 500, which is correct: every expected
// refusal on these routes is one.
func mapApplicationError(err error) error {
	var re *domain.ReasonError
	if !errors.As(err, &re) {
		return err
	}

	message := re.Message
	if message == "" {
		message = applicationReasonMessages[re.Reason]
	}
	if message == "" {
		message = re.Err.Error()
	}

	var name, code string
	switch {
	case errors.Is(re.Err, domain.ErrNotFound):
		name, code = "NotFound", "404"
	case errors.Is(re.Err, domain.ErrInvalidRequest):
		name, code = "BadRequest", "400"
	case errors.Is(re.Err, domain.ErrVersionMismatch):
		name, code = "VersionMismatch", "412"
	default:
		return err
	}

	return &svc.ApplicationError{Name: name, Code: code, Message: message, Reason: re.Reason}
}
