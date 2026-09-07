// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package service is the dependency-injection layer: one XxxImpl(ctx) per
// port, switching on REPOSITORY_SOURCE between the Postgres adapter and its
// mock double. This mirrors lfx-v2-committee-service's
// cmd/committee-api/service/providers.go exactly, rather than a fixed
// internal/container/container.go with no mock path (plan.md Finding 4).
package service

import (
	"context"
	"log"
	"log/slog"
	"os"
	"sync"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/auth"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/postgres"
	usecaseSvc "github.com/linuxfoundation/lfx-v2-formation-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// repositorySourceEnv selects the Postgres adapter or its mock double for
// every storage port. Committee-service's equivalent env vars
// (REPOSITORY_SOURCE, MESSAGING_SOURCE, AUTH_SOURCE) are each read
// independently per concern; formation-service's storage concern covers all
// four repositories, so one variable is enough for them.
const repositorySourceEnv = "REPOSITORY_SOURCE"

// authSourceEnv selects the real Heimdall JWKS validator or the mock double.
// It defaults to "jwt": the mock is opt-in only, never a fallback.
const authSourceEnv = "AUTH_SOURCE"

var (
	pgDB       *postgres.DB
	pgDoOnce   sync.Once
	pgInitFail error
)

func repositorySource() string {
	src := os.Getenv(repositorySourceEnv)
	if src == "" {
		src = "postgres"
	}
	return src
}

// postgresImpl lazily connects the shared pool, applying the schema exactly
// once regardless of how many *Impl functions are called.
func postgresImpl(ctx context.Context, cfg *config.Config) *postgres.DB {
	pgDoOnce.Do(func() {
		slog.InfoContext(ctx, "connecting to postgres", "database", cfg.Database.Redacted())
		pgDB, pgInitFail = postgres.Connect(ctx, cfg.Database.DSN())
		if pgInitFail != nil {
			log.Fatalf("failed to connect to postgres: %v", pgInitFail)
		}
	})
	return pgDB
}

// FormationRepositoryImpl initializes the formation repository implementation
// based on REPOSITORY_SOURCE.
func FormationRepositoryImpl(ctx context.Context, cfg *config.Config) port.FormationRepository {
	switch repositorySource() {
	case "mock":
		slog.InfoContext(ctx, "initializing mock formation repository")
		return mock.NewFormationRepository()
	case "postgres":
		slog.InfoContext(ctx, "initializing postgres formation repository")
		return postgres.NewFormationRepo(postgresImpl(ctx, cfg).Bun)
	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE: %s", repositorySource())
	}
	return nil
}

// ItemRepositoryImpl initializes the item repository implementation based on
// REPOSITORY_SOURCE.
func ItemRepositoryImpl(ctx context.Context, cfg *config.Config) port.ItemRepository {
	switch repositorySource() {
	case "mock":
		slog.InfoContext(ctx, "initializing mock item repository")
		return mock.NewItemRepository()
	case "postgres":
		slog.InfoContext(ctx, "initializing postgres item repository")
		return postgres.NewItemRepo(postgresImpl(ctx, cfg).Bun)
	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE: %s", repositorySource())
	}
	return nil
}

// ActivityRepositoryImpl initializes the activity repository implementation
// based on REPOSITORY_SOURCE.
func ActivityRepositoryImpl(ctx context.Context, cfg *config.Config) port.ActivityRepository {
	switch repositorySource() {
	case "mock":
		slog.InfoContext(ctx, "initializing mock activity repository")
		return mock.NewActivityRepository()
	case "postgres":
		slog.InfoContext(ctx, "initializing postgres activity repository")
		return postgres.NewActivityRepo(postgresImpl(ctx, cfg).Bun)
	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE: %s", repositorySource())
	}
	return nil
}

// TemplateRepositoryImpl initializes the template repository implementation
// based on REPOSITORY_SOURCE.
func TemplateRepositoryImpl(ctx context.Context, cfg *config.Config) port.TemplateRepository {
	switch repositorySource() {
	case "mock":
		slog.InfoContext(ctx, "initializing mock template repository")
		return mock.NewTemplateRepository()
	case "postgres":
		slog.InfoContext(ctx, "initializing postgres template repository")
		return postgres.NewTemplateRepo(postgresImpl(ctx, cfg).Bun)
	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE: %s", repositorySource())
	}
	return nil
}

// ProjectReaderImpl returns the NATS request/reply project reader. Returns
// nil until that adapter lands (tracked separately) — a nil reader degrades
// announcement-date lookups rather than erroring, which is the conservative
// answer for a dependency that does not exist yet.
func ProjectReaderImpl(_ context.Context, _ *config.Config) port.ProjectReader {
	return nil
}

// AuthServiceImpl initializes the authentication service implementation based
// on AUTH_SOURCE. An unset value means "jwt", and any failure to build the
// real validator is fatal rather than a silent downgrade to the mock.
func AuthServiceImpl(ctx context.Context, cfg *config.Config) port.Authenticator {
	authSource := os.Getenv(authSourceEnv)
	if authSource == "" {
		authSource = "jwt"
	}

	switch authSource {
	case "mock":
		slog.InfoContext(ctx, "initializing mock authentication service")
		return mock.NewAuthService()
	case "jwt":
		slog.InfoContext(ctx, "initializing JWT authentication service")
		mockLocalPrincipal := os.Getenv("JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL")
		if mockLocalPrincipal != "" && os.Getenv(constants.EnvJWTMockBypassConfirm) != "true" {
			// A single stray env var (chart app.extraEnv, a copy-pasted
			// ArgoCD override) must not be able to authenticate every
			// request as a fixed principal. Require a second,
			// independent confirmation before honoring it under the
			// real "jwt" auth source.
			log.Fatalf("%s is set but %s is not \"true\": refusing to bypass JWT validation under AUTH_SOURCE=jwt",
				"JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL", constants.EnvJWTMockBypassConfirm)
		}
		jwtConfig := auth.JWTAuthConfig{
			JWKSURL:            cfg.JWKSUrl,
			Audience:           cfg.Audience,
			Issuer:             cfg.Issuer,
			MockLocalPrincipal: mockLocalPrincipal,
			MockLocalEmail:     os.Getenv("JWT_AUTH_DISABLED_MOCK_LOCAL_EMAIL"),
		}
		if jwtConfig.JWKSURL == "" || jwtConfig.Audience == "" {
			log.Fatalf("JWT configuration incomplete: %s and %s are required",
				constants.EnvJWKSURL, constants.EnvAudience)
		}
		jwtAuth, err := auth.NewJWTAuth(jwtConfig)
		if err != nil {
			log.Fatalf("failed to initialize JWT authentication service: %v", err)
		}
		return jwtAuth
	default:
		log.Fatalf("unsupported %s: %s", authSourceEnv, authSource)
	}
	return nil
}

// New builds the wired service. Returns a close function that releases the
// Postgres pool when REPOSITORY_SOURCE=postgres connected one, and a no-op
// otherwise.
func New(ctx context.Context, cfg *config.Config) (*usecaseSvc.Service, func() error, error) {
	slog.InfoContext(ctx, "wiring service dependencies", "repository_source", repositorySource())

	authService := AuthServiceImpl(ctx, cfg)
	formations := FormationRepositoryImpl(ctx, cfg)
	items := ItemRepositoryImpl(ctx, cfg)
	activity := ActivityRepositoryImpl(ctx, cfg)
	templates := TemplateRepositoryImpl(ctx, cfg)
	projects := ProjectReaderImpl(ctx, cfg)

	// db is nil in mock mode: readiness then reports OK unconditionally,
	// which is the documented "not wired yet" behaviour in service.go.
	var db interface {
		Ping(ctx context.Context) error
	}
	if repositorySource() == "postgres" {
		db = postgresImpl(ctx, cfg)
	}

	svc := usecaseSvc.NewService(
		usecaseSvc.WithDB(db),
		usecaseSvc.WithAuth(authService),
		usecaseSvc.WithFormations(formations),
		usecaseSvc.WithItems(items),
		usecaseSvc.WithActivity(activity),
		usecaseSvc.WithTemplates(templates),
		usecaseSvc.WithProjects(projects),
	)

	closeFn := func() error {
		if pgDB != nil {
			return pgDB.Close()
		}
		return nil
	}

	slog.InfoContext(ctx, "service dependencies wired")
	return svc, closeFn, nil
}
