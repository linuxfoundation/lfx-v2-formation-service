// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/log"
)

// The one refusal these routes add to the intake reasons they share. It is
// not a rule about applications — a uuid that will not parse names no row, so
// there is nothing to look up.
const reasonApplicationUIDBad = "application_uid_invalid"

// ReviseApplication replaces the answers on an application.
//
// No audience appears anywhere in this function, and that is the point. The
// submitter and the formation team both reach it, the gateway having resolved
// `writer` on the application object for either of them, and the model grants
// that relation to both. Branching on who the caller is here would put the
// same rule in a second place, where the two copies drift.
func (s *Service) ReviseApplication(
	ctx context.Context, p *svc.ReviseApplicationPayload,
) (*svc.ProjectApplicationMutationResult, error) {
	return s.mutateApplication(ctx, "revise-application", p.UID, p.IfMatch, func(
		ctx context.Context, tx port.Tx, current *model.Application,
	) (*model.Application, error) {
		answers, err := validateAnswers(p.Application)
		if err != nil {
			return nil, err
		}
		next := *current
		next.Payload = answers
		next.Revision++
		next.UpdatedAt = time.Now().UTC()
		if err := s.validateApplicationProjection(&next); err != nil {
			return nil, err
		}
		// The previous answers are overwritten. Payload versioning was not
		// requested, and applications have no history table.
		return tx.Applications().UpdatePayload(ctx, current.UID, p.IfMatch, answers)
	})
}

// WithdrawApplication takes an application back.
//
// The record is kept rather than removed. Withdrawing and deleting are
// different operations with different endpoints.
func (s *Service) WithdrawApplication(
	ctx context.Context, p *svc.WithdrawApplicationPayload,
) (*svc.ProjectApplicationMutationResult, error) {
	return s.mutateApplication(ctx, "withdraw-application", p.UID, p.IfMatch, func(
		ctx context.Context, tx port.Tx, current *model.Application,
	) (*model.Application, error) {
		return tx.Applications().Transition(
			ctx, current.UID, p.IfMatch, model.ApplicationWithdrawn,
		)
	})
}

// DeleteApplication removes an application from storage and the search index,
// and asks access sync to remove publisher-managed user grants. Access sync
// deliberately preserves team-subject tuples, including formation_team.
//
// One endpoint for both audiences, like revise and withdraw: the gateway
// resolves `writer` and the model grants it to the submitter and the
// formation team alike, so nothing here asks who is calling.
//
// No state gate. No source enumerates a state set or says which actions are
// legal in which state, so refusing a delete on a decided application would
// be this service inventing a rule. Deleting and denying are different
// requests — denying keeps the record, deleting removes it.
//
// Cleanup is attempted after the delete commits. A durable deletion marker
// lets reconciliation retry either downstream removal after this returns.
func (s *Service) DeleteApplication(ctx context.Context, p *svc.DeleteApplicationPayload) error {
	if s.uow == nil {
		slog.ErrorContext(ctx, "formationService.delete-application: no unit of work wired")
		return errors.New("application storage is not available")
	}

	uid, err := uuid.Parse(p.UID)
	if err != nil {
		return mapApplicationError(
			domain.NewReasonError(domain.ErrInvalidRequest, reasonApplicationUIDBad))
	}

	if err := s.uow.Do(ctx, func(tx port.Tx) error {
		current, err := tx.Applications().GetForUpdate(ctx, uid)
		if err != nil {
			return err
		}
		if current.Revision != p.IfMatch {
			return domain.NewReasonError(domain.ErrVersionMismatch, reasonVersionMismatch)
		}
		if _, err := tx.Applications().Delete(ctx, uid, p.IfMatch); err != nil {
			return err
		}
		return nil
	}); err != nil {
		slog.ErrorContext(ctx, "formationService.delete-application", "application_uid", uid, log.ErrKey, err)
		return mapApplicationError(withApplicationReason(err))
	}

	_ = s.deleteApplicationProjection(ctx, uid)
	slog.InfoContext(ctx, "formationService.delete-application", "application_uid", uid)
	return nil
}

// mutateApplication commits an application write, then republishes it.
//
// The locking read serializes database mutations; If-Match binds the write to
// the revision the caller read from the private projection.
func (s *Service) mutateApplication(
	ctx context.Context,
	operation string,
	rawUID string,
	ifMatch int64,
	mutate func(context.Context, port.Tx, *model.Application) (*model.Application, error),
) (*svc.ProjectApplicationMutationResult, error) {
	if s.uow == nil {
		// A deployment fault, not a refusal the caller can act on.
		slog.ErrorContext(ctx, "formationService."+operation+": no unit of work wired")
		return nil, errors.New("application storage is not available")
	}

	uid, err := uuid.Parse(rawUID)
	if err != nil {
		return nil, mapApplicationError(
			domain.NewReasonError(domain.ErrInvalidRequest, reasonApplicationUIDBad))
	}

	var updated *model.Application
	err = s.uow.Do(ctx, func(tx port.Tx) error {
		current, err := tx.Applications().GetForUpdate(ctx, uid)
		if err != nil {
			return err
		}
		if current.Revision != ifMatch {
			// A refused write publishes nothing. Publishing the current row
			// here would race a concurrent delete's tombstone; the repair
			// sweep refreshes a stale projection instead.
			return domain.NewReasonError(domain.ErrVersionMismatch, reasonVersionMismatch)
		}
		updated, err = mutate(ctx, tx, current)
		if err != nil {
			return err
		}
		return s.validateApplicationProjection(updated)
	})
	if err != nil {
		slog.ErrorContext(ctx, "formationService."+operation, "application_uid", uid, log.ErrKey, err)
		return nil, mapApplicationError(withApplicationReason(err))
	}

	_ = s.publishApplication(ctx, updated)
	slog.InfoContext(ctx, "formationService."+operation,
		"application_uid", updated.UID,
		"state", updated.State,
	)
	application := applicationToWire(updated)
	etag := strconv.FormatInt(updated.Revision, 10)
	return &svc.ProjectApplicationMutationResult{Application: application, Etag: &etag}, nil
}

func (s *Service) deleteApplicationProjection(ctx context.Context, uid uuid.UUID) error {
	var publishErrs []error
	if s.applicationAccess != nil {
		if err := s.applicationAccess.DeleteApplicationAccess(ctx, uid.String()); err != nil {
			publishErrs = append(publishErrs, err)
			slog.ErrorContext(ctx, "formationService.delete-application: revoking access failed",
				"application_uid", uid, log.ErrKey, err)
		}
	} else {
		publishErrs = append(publishErrs, errors.New("application access publisher is not configured"))
	}
	if s.applicationIndexer != nil {
		if err := s.applicationIndexer.DeleteApplication(ctx, uid.String()); err != nil {
			publishErrs = append(publishErrs, err)
			slog.ErrorContext(ctx, "formationService.delete-application: removing the document failed",
				"application_uid", uid, log.ErrKey, err)
		}
	} else {
		publishErrs = append(publishErrs, errors.New("application indexer is not configured"))
	}
	return errors.Join(publishErrs...)
}
