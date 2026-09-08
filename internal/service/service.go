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
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
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
// the service package stays free of a driver import.
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
	// which is the conservative answer (readiness requires one) rather
	// than an error — the read path must not depend on a dependency that
	// does not exist yet.
	projects port.ProjectReader
}

// Ensure Service satisfies the generated service and authorization interfaces.
var (
	_ svc.Service = (*Service)(nil)
	_ svc.Auther  = (*Service)(nil)
)

// serviceOption sets one field on a Service under construction.
type serviceOption func(*Service)

// WithDB wires the readiness probe's database handle. Omitting it makes
// readiness report OK unconditionally (the health-only scaffold and tests
// that don't stand up a database).
func WithDB(db dbPinger) serviceOption {
	return func(s *Service) { s.db = db }
}

// WithAuth wires the JWT authenticator. Omitting it fails every secured
// request closed rather than admitting it.
func WithAuth(auth port.Authenticator) serviceOption {
	return func(s *Service) { s.auth = auth }
}

// WithFormations wires the formation repository.
func WithFormations(formations port.FormationRepository) serviceOption {
	return func(s *Service) { s.formations = formations }
}

// WithItems wires the item repository.
func WithItems(items port.ItemRepository) serviceOption {
	return func(s *Service) { s.items = items }
}

// WithActivity wires the activity repository.
func WithActivity(activity port.ActivityRepository) serviceOption {
	return func(s *Service) { s.activity = activity }
}

// WithTemplates wires the template repository.
func WithTemplates(templates port.TemplateRepository) serviceOption {
	return func(s *Service) { s.templates = templates }
}

// WithProjects wires the project reader. Omitting it degrades
// announcement-date lookups to nil rather than erroring, which is the
// conservative answer until the NATS adapter is wired.
func WithProjects(projects port.ProjectReader) serviceOption {
	return func(s *Service) { s.projects = projects }
}

// NewService constructs a Service from the given options. Any dependency
// left unset stays nil, and each field's own nil-handling (documented on the
// Service struct) decides what that means, so adding a new dependency later
// is not a call-site-wide signature change.
func NewService(opts ...serviceOption) *Service {
	s := &Service{}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// unauthorizedError is the declared 401 for every JWTAuth failure. Sharing
// one constructor keeps the message consistent and avoids leaking
// parse-error detail (token contents, library internals) to the caller.
func unauthorizedError() *svc.UnauthorizedError {
	return &svc.UnauthorizedError{Code: "401", Message: "missing, expired, or malformed bearer token"}
}

// JWTAuth implements the authorization logic for service
// "lfx_v2_formation_service" for the "jwt" security scheme.
func (s *Service) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	if s.auth == nil {
		// Server misconfiguration, not a client credential problem: leave
		// this as a plain error so it falls through Goa's default formatter
		// as a 500, distinct from the declared 401 below for actual token
		// failures.
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
		if errors.Is(err, domain.ErrAuthUnavailable) {
			// The JWKS endpoint was unreachable, not a bad credential:
			// same rationale as the nil-authenticator branch above, leave
			// this as a plain error so it falls through as a 500 instead
			// of telling a valid caller their token is bad.
			return ctx, errors.New("authentication service unavailable")
		}
		// Returned as the declared UnauthorizedError, not the raw parse
		// error: an error that isn't one of the method's declared error
		// types falls through Goa's default formatter as a 500, so an
		// invalid or expired token would otherwise be reported as an
		// internal server failure rather than 401.
		return ctx, unauthorizedError()
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
