// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package service provides the service implementations.
package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"goa.design/goa/v3/security"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/log"
)

// readyzPingTimeout bounds how long the readiness probe waits on the
// database before reporting unavailable, so a stalled connection fails the
// probe promptly rather than hanging it.
const readyzPingTimeout = 2 * time.Second

// dbPinger is the one method the readiness probe needs from the database
// handle. Declared here rather than imported from infrastructure/postgres so
// the service package stays free of a driver import (FR-041).
type dbPinger interface {
	Ping(ctx context.Context) error
}

// Service implements the generated service interface.
type Service struct {
	// db is nil in tests that do not exercise readiness against a real
	// database; a nil check is the "not wired yet" case, not an error.
	db dbPinger

	// auth validates the Heimdall JWT on every secured method. A nil
	// authenticator rejects every secured request rather than admitting it,
	// so an unwired dependency cannot become an authentication bypass.
	auth port.Authenticator

	formations port.FormationRepository
	items      port.ItemRepository
	activity   port.ActivityRepository
	templates  port.TemplateRepository

	// projects is nil until the NATS request/reply adapter lands. A nil
	// reader degrades the checklist read path to "no announcement date",
	// which is the FR-019-correct answer (readiness requires one) rather
	// than an error — the read path must not depend on a dependency that
	// does not exist yet.
	projects port.ProjectReader
}

// Ensure Service satisfies the generated service and authorization interfaces.
var (
	_ svc.Service = (*Service)(nil)
	_ svc.Auther  = (*Service)(nil)
)

// NewService constructs a Service. db and projects may be nil: db=nil makes
// readiness report OK unconditionally (the health-only scaffold and tests
// that don't stand up a database); projects=nil degrades announcement-date
// lookups rather than erroring, until the NATS adapter is wired.
func NewService(
	db dbPinger,
	auth port.Authenticator,
	formations port.FormationRepository,
	items port.ItemRepository,
	activity port.ActivityRepository,
	templates port.TemplateRepository,
	projects port.ProjectReader,
) *Service {
	return &Service{
		db:         db,
		auth:       auth,
		formations: formations,
		items:      items,
		activity:   activity,
		templates:  templates,
		projects:   projects,
	}
}

// JWTAuth implements the authorization logic for service
// "lfx_v2_formation_service" for the "jwt" security scheme.
func (s *Service) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	if s.auth == nil {
		slog.ErrorContext(ctx, "formationService.jwt-auth: no authenticator wired")
		return ctx, errors.New("authentication is not available")
	}

	// Parse the Heimdall-authorized principal and email from the token.
	// Email is non-empty for user JWTs (set by Authelia's oidc_contextualizer); empty for M2M/anonymous.
	principal, email, err := s.auth.ParsePrincipal(ctx, token, slog.Default())
	if err != nil {
		slog.ErrorContext(ctx, "formationService.jwt-auth",
			log.ErrKey, err,
			"token_length", len(token),
		)
		return ctx, err
	}

	ctx = context.WithValue(ctx, constants.PrincipalContextID, principal)
	ctx = context.WithValue(ctx, constants.EmailContextID, email)
	return ctx, nil
}

// ServiceReady reports whether the service is able to accept inbound
// requests. Pings the database when one is wired; future dependency checks
// (messaging, etc.) should be combined here with logical AND.
func (s *Service) ServiceReady(ctx context.Context) bool {
	if s.db == nil {
		return true
	}
	pingCtx, cancel := context.WithTimeout(ctx, readyzPingTimeout)
	defer cancel()
	return s.db.Ping(pingCtx) == nil
}

// Readyz implements the readiness probe.
func (s *Service) Readyz(ctx context.Context) ([]byte, error) {
	if !s.ServiceReady(ctx) {
		return nil, &svc.ServiceUnavailableError{
			Code:    "503",
			Message: "The service is unavailable.",
		}
	}
	return []byte("OK\n"), nil
}

// Livez implements the liveness probe.
func (s *Service) Livez(_ context.Context) ([]byte, error) {
	// This always returns OK as long as the service is still running. As this
	// endpoint is used as a Kubernetes liveness check, the service must
	// self-detect non-recoverable errors and self-terminate.
	return []byte("OK\n"), nil
}
