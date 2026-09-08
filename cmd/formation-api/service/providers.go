// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package service is the dependency-injection layer: one XxxImpl(ctx) per
// port, switching on REPOSITORY_SOURCE between the Postgres adapter and its
// mock double. This mirrors lfx-v2-committee-service's
// cmd/committee-api/service/providers.go exactly, rather than a fixed
// container with no mock path: being able to run the whole service against
// in-memory doubles is what makes local development and the service-level
// tests possible without a database.
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

// UnitOfWorkImpl wires the transaction UpdateItem commits an item change and
// its activity entry through. In mock mode it must share the same repository
// instances formations, items and activity already reference — a fresh set
// of mock repositories would make writes inside the unit of work invisible
// to reads outside it — so it takes them as concrete types rather than
// deriving its own.
func UnitOfWorkImpl(
	ctx context.Context,
	cfg *config.Config,
	formations port.FormationRepository,
	items port.ItemRepository,
	activity port.ActivityRepository,
	templates port.TemplateRepository,
) port.UnitOfWork {
	switch repositorySource() {
	case "mock":
		slog.InfoContext(ctx, "initializing mock unit of work")
		return mock.NewUnitOfWork(
			formations.(*mock.FormationRepository),
			items.(*mock.ItemRepository),
			activity.(*mock.ActivityRepository),
			templates.(*mock.TemplateRepository),
		)
	case "postgres":
		slog.InfoContext(ctx, "initializing postgres unit of work")
		return postgres.NewUnitOfWork(postgresImpl(ctx, cfg).Bun)
	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE: %s", repositorySource())
	}
	return nil
}

// ProjectReaderImpl returns the project reader, and returns nil because there
// is nothing yet that can implement it.
//
// The NATS transport and a project client now exist
// (internal/infrastructure/nats), but the project service exposes no subject
// that returns the settings record — only per-attribute lookups, of which
// writers is the only role. GetSettings needs the announcement date and the
// auditors list from that record, so it cannot be answered without an upstream
// addition.
//
// Wiring a reader that filled writers and left auditors empty would be worse
// than wiring none: assignment validation refuses anyone outside writers ∪
// auditors, so it would start rejecting every legitimate auditor. A nil reader
// keeps that check inert, which is wrong-but-harmless rather than harmful.
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
	uow := UnitOfWorkImpl(ctx, cfg, formations, items, activity, templates)
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
		usecaseSvc.WithUnitOfWork(uow),
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

// StartReconcile starts the reconcile loop, and reports whether it started.
//
// The loop is the mechanism of record for creating checklists, so it runs on
// every replica with no leader election — duplicate creation is absorbed by the
// uniqueness constraint on project_uid.
//
// It is not started when nothing can list forming projects. A loop that woke up
// every fifteen minutes to sweep an empty list would log its way through the
// retention window saying nothing useful, and the absence is worth stating once
// at startup instead.
func StartReconcile(ctx context.Context, cfg *config.Config) bool {
	projects := ProjectReaderImpl(ctx, cfg)
	if projects == nil {
		slog.WarnContext(ctx, "reconcile loop not started: nothing can list forming projects yet, "+
			"so no checklist is created automatically. Use formation-cli expand in the meantime")
		return false
	}

	formations := FormationRepositoryImpl(ctx, cfg)
	items := ItemRepositoryImpl(ctx, cfg)
	activity := ActivityRepositoryImpl(ctx, cfg)
	templates := TemplateRepositoryImpl(ctx, cfg)
	uow := UnitOfWorkImpl(ctx, cfg, formations, items, activity, templates)

	reconciler := usecaseSvc.NewReconciler(
		projects,
		usecaseSvc.NewExpander(usecaseSvc.NewTemplateSelector(templates), uow, projects),
		usecaseSvc.NewLifecycler(formations),
	)

	go reconciler.Run(ctx)
	return true
}
