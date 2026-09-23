// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/log"
)

// Reasons the intake route can refuse a payload.
const (
	reasonSubmitterUsernameRequired = "submitter_username_required"
	reasonProjectWebsiteBad         = "project_website_invalid"
	reasonFormationListInvalid      = "formation_list_invalid"
)

var applicationReasonMessages = map[string]string{
	reasonNotFound:                  "no such application",
	reasonVersionMismatch:           "if-match did not match the application's current revision",
	reasonSubmitterUsernameRequired: "submitter_username is required",
	reasonProjectWebsiteBad:         "project_website must be an http or https URL",
	reasonFormationListInvalid:      "formation_list must be a list of email addresses",
	reasonApplicationUIDBad:         "the application identifier is not a uuid",
}

// Intake payload keys this service knows about by name. Everything else in
// the map is carried through untouched.
const (
	payloadProjectName    = "project_name"
	payloadProjectWebsite = "project_website"
	payloadFormationList  = "formation_list"
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

	created, err := s.applications.Create(ctx, &model.Application{
		State:             model.ApplicationSubmitted,
		SubmitterUsername: p.SubmitterUsername,
		SubmitterName:     p.SubmitterName,
		SubmitterEmail:    p.SubmitterEmail,
		TargetParentUID:   p.TargetParentUID,
		Payload:           application,
	})
	if err != nil {
		slog.ErrorContext(ctx, "formationService.create-application", log.ErrKey, err)
		return nil, err
	}

	slog.InfoContext(ctx, "formationService.create-application",
		"application_uid", created.UID,
		// The applicant's name and email are deliberately not logged. They
		// are PII on a record that is not public, and a log line is the one
		// copy of them nothing here can later delete.
		"submitter_username", created.SubmitterUsername,
		"has_target_parent", created.TargetParentUID != nil,
	)

	s.publishApplication(ctx, created)

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
// Neither failure is returned. The record is already committed, and a caller
// told "your submission failed" would file it again, producing two
// applications where the first one was fine. What the caller loses instead is
// visibility, which the next revise or decision restores by republishing
// both. That is the same trade the other publishers here make, and it is why
// these are logged at error rather than swallowed.
func (s *Service) publishApplication(ctx context.Context, a *model.Application) {
	if s.applicationAccess == nil || s.applicationTeam == "" {
		slog.ErrorContext(ctx, "formationService.publish-application: no access publisher wired; the application is readable by nobody",
			"application_uid", a.UID,
		)
	} else if err := s.applicationAccess.PublishApplicationAccess(ctx, port.ApplicationAccess{
		ApplicationUID:    a.UID.String(),
		SubmitterUsername: a.SubmitterUsername,
		FormationTeam:     s.applicationTeam,
	}); err != nil {
		slog.ErrorContext(ctx, "formationService.publish-application: granting access failed",
			"application_uid", a.UID, log.ErrKey, err,
		)
	}

	if s.applicationIndexer == nil {
		return
	}
	if err := s.applicationIndexer.PublishApplication(ctx, applicationProjection(a)); err != nil {
		slog.ErrorContext(ctx, "formationService.publish-application: indexing failed",
			"application_uid", a.UID, log.ErrKey, err,
		)
	}
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

// validateIntake checks the envelope and returns the answers to store.
//
// The source names the intake fields but does not define their wire keys,
// types or requiredness. Only the two approved product shapes are enforced:
// the project website and formation-contact email addresses.
func validateIntake(p *svc.CreateApplicationPayload) (map[string]any, error) {
	p.SubmitterUsername = strings.TrimSpace(p.SubmitterUsername)
	if p.SubmitterUsername == "" {
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
	// The website stands in for the logo the mockup asked for, so it is the
	// one intake answer whose shape is checked here.
	if raw, present := answers[payloadProjectWebsite]; present {
		website, ok := raw.(string)
		if !ok || !isSafeURL(strings.TrimSpace(website)) {
			return nil, domain.NewReasonError(domain.ErrInvalidRequest, reasonProjectWebsiteBad)
		}
	}

	if err := validateFormationList(answers[payloadFormationList]); err != nil {
		return nil, err
	}

	return answers, nil
}

// validateFormationList checks that the people named for the formation work
// are email addresses.
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
		// Deliberately shallow: an at-sign with something either side. A
		// stricter parser refuses addresses that are perfectly deliverable,
		// and nothing here sends mail, so the cost of a wrong refusal is
		// higher than the cost of a wrong acceptance.
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
