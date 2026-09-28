// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/service/email"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/log"
)

// dispatchApplicationSubmittedEmails sends the submission receipt to the
// submitter and a review-queue alert to the formation team. Both sends are
// best-effort: a failure is logged and never blocks the API response.
//
// MarkApplicationNotified enforces at-most-once delivery across replicas: the
// column is claimed before the send attempt, so a failed or slow send is NOT
// retried — the timestamp records a claimed attempt, not a confirmed delivery.
func (s *Service) dispatchApplicationSubmittedEmails(ctx context.Context, a *model.Application) {
	if s.emailer == nil || !s.emailCfg.Enabled {
		return
	}

	projectName := applicationProjectName(a)

	// Submitter notification — MarkApplicationNotified ensures at most one
	// replica sends per application even when multiple pods run concurrently.
	acquired, err := s.applications.MarkApplicationNotified(ctx, a.UID, "notified_submitted_at")
	if err != nil {
		slog.WarnContext(ctx, "formationService.dispatch-application-email: mark submitted failed",
			"application_uid", a.UID, log.ErrKey, err)
	}
	if acquired {
		appURL := s.emailCfg.AdminBaseURL + "/formations?tab=proposals"
		subject, html, text, renderErr := email.RenderApplicationSubmitted(email.ApplicationSubmittedData{
			RecipientName:  a.SubmitterName,
			ProjectName:    projectName,
			ApplicationURL: appURL,
			FormationEmail: s.emailCfg.FormationInbox,
		})
		if renderErr != nil {
			slog.ErrorContext(ctx, "formationService.dispatch-application-email: render submitted failed",
				"application_uid", a.UID, log.ErrKey, renderErr)
		} else if sendErr := s.emailer.Send(ctx, port.EmailMessage{
			To: a.SubmitterEmail, Subject: subject, HTML: html, Text: text,
		}); sendErr != nil {
			slog.ErrorContext(ctx, "formationService.dispatch-application-email: send submitted failed",
				"application_uid", a.UID, log.ErrKey, sendErr)
		}

		// Formation team notification — gated on the same acquired claim so
		// only one replica sends the alert per application submission.
		if s.emailCfg.FormationInbox != "" {
			teamSubject, teamHTML, teamText, renderErr := email.RenderApplicationSubmittedTeam(email.ApplicationSubmittedTeamData{
				ProjectName:    projectName,
				ReviewQueueURL: s.emailCfg.AdminBaseURL + "/foundation/formations?tab=proposals",
			})
			if renderErr != nil {
				slog.ErrorContext(ctx, "formationService.dispatch-application-email: render submitted-team failed",
					"application_uid", a.UID, log.ErrKey, renderErr)
			} else if sendErr := s.emailer.Send(ctx, port.EmailMessage{
				To: s.emailCfg.FormationInbox, Subject: teamSubject, HTML: teamHTML, Text: teamText,
			}); sendErr != nil {
				slog.ErrorContext(ctx, "formationService.dispatch-application-email: send submitted-team failed",
					"application_uid", a.UID, log.ErrKey, sendErr)
			}
		}
	}
}

// dispatchApplicationDecidedEmail sends the acceptance or denial notification
// to the submitter. It looks up the application after the commit so the caller
// does not need to thread it through the return path.
//
// Delivery is at most once: MarkApplicationNotified claims the column before
// the send, so a failed send is not retried on a subsequent call.
func (s *Service) dispatchApplicationDecidedEmail(ctx context.Context, rawUID string, decision model.ApplicationState) {
	if s.emailer == nil || !s.emailCfg.Enabled {
		return
	}
	uid, err := uuid.Parse(rawUID)
	if err != nil {
		return // already validated by the caller; unreachable in practice
	}
	a, err := s.applications.Get(ctx, uid)
	if err != nil {
		slog.WarnContext(ctx, "formationService.dispatch-application-email: get application failed",
			"application_uid", uid, log.ErrKey, err)
		return
	}

	projectName := applicationProjectName(a)

	var column, subject, html, text string
	var renderErr error
	switch decision {
	case model.ApplicationAccepted:
		column = "notified_accepted_at"
		subject, html, text, renderErr = email.RenderApplicationAccepted(email.ApplicationAcceptedData{
			RecipientName:  a.SubmitterName,
			ProjectName:    projectName,
			FormationEmail: s.emailCfg.FormationInbox,
		})
	case model.ApplicationDenied:
		column = "notified_denied_at"
		subject, html, text, renderErr = email.RenderApplicationDenied(email.ApplicationDeniedData{
			RecipientName:  a.SubmitterName,
			ProjectName:    projectName,
			FormationEmail: s.emailCfg.FormationInbox,
		})
	default:
		return
	}

	if renderErr != nil {
		slog.ErrorContext(ctx, "formationService.dispatch-application-email: render failed",
			"application_uid", uid, "state", decision, log.ErrKey, renderErr)
		return
	}

	acquired, err := s.applications.MarkApplicationNotified(ctx, uid, column)
	if err != nil {
		slog.WarnContext(ctx, "formationService.dispatch-application-email: mark failed",
			"application_uid", uid, "column", column, log.ErrKey, err)
		return
	}
	if !acquired {
		return // another replica already sent the email
	}

	if sendErr := s.emailer.Send(ctx, port.EmailMessage{
		To: a.SubmitterEmail, Subject: subject, HTML: html, Text: text,
	}); sendErr != nil {
		slog.ErrorContext(ctx, "formationService.dispatch-application-email: send failed",
			"application_uid", uid, "state", decision, log.ErrKey, sendErr)
	}
}

// applicationProjectName extracts the proposed project name from the payload
// or falls back to the UID string so the email always has something readable.
func applicationProjectName(a *model.Application) string {
	if name, ok := a.Payload["project_name"].(string); ok && name != "" {
		return name
	}
	return a.UID.String()
}
