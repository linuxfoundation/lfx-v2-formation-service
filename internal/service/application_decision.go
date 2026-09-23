// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// AcceptApplication records that an application was accepted.
//
// It changes the application state and does not create a project.
func (s *Service) AcceptApplication(
	ctx context.Context, p *svc.AcceptApplicationPayload,
) (*svc.ProjectApplicationMutationResult, error) {
	return s.decideApplication(
		ctx, "accept-application", p.UID, p.IfMatch, model.ApplicationAccepted,
	)
}

// DenyApplication records that an application was denied.
//
// It changes the application state and retains the record.
func (s *Service) DenyApplication(
	ctx context.Context, p *svc.DenyApplicationPayload,
) (*svc.ProjectApplicationMutationResult, error) {
	return s.decideApplication(
		ctx, "deny-application", p.UID, p.IfMatch, model.ApplicationDenied,
	)
}

// decideApplication records one decision against an application.
//
// Both decisions are the same write with a different target state, so they
// share the row lock, state transition and republish path. There is no state
// gate and no application decision actor.
func (s *Service) decideApplication(
	ctx context.Context, operation string, rawUID string, ifMatch int64,
	decision model.ApplicationState,
) (*svc.ProjectApplicationMutationResult, error) {
	return s.mutateApplication(ctx, operation, rawUID, ifMatch, func(
		ctx context.Context, tx port.Tx, current *model.Application,
	) (*model.Application, error) {
		return tx.Applications().Transition(ctx, current.UID, ifMatch, decision)
	})
}
