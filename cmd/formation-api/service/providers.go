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
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/nats"
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

	natsClient   *nats.Client
	natsDoOnce   sync.Once
	natsInitFail error
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

// natsImpl lazily connects the shared NATS client, once regardless of how many
// callers ask for it.
//
// Unlike Postgres, a failure here is not fatal. This service answers both its
// read and its write routes without NATS: what depends on it is assignment
// validation, due-date resolution and the reconcile sweep, all of which degrade
// to a documented behaviour when the reader is absent. Exiting instead would
// take the API down over an upstream this service can survive without, and the
// client reconnects on its own once NATS returns.
func natsImpl(ctx context.Context, cfg *config.Config) *nats.Client {
	natsDoOnce.Do(func() {
		slog.InfoContext(ctx, "connecting to NATS", "url", cfg.NATSUrl)
		natsClient, natsInitFail = nats.New(ctx, nats.Config{URL: cfg.NATSUrl})
		if natsInitFail != nil {
			slog.ErrorContext(ctx, "could not connect to NATS; assignment validation stays inert, "+
				"due dates are left unset and the reconcile loop sweeps nothing",
				"url", cfg.NATSUrl, "error", natsInitFail)
		}
	})
	return natsClient
}

// ProjectReaderImpl returns the project reader over NATS request/reply.
//
// Returns nil when NATS could not be reached, and that nil is load-bearing
// rather than an oversight: every consumer checks for it and has a defined
// behaviour without a reader — assignment validation accepts rather than
// refusing everyone, due dates are left unset rather than guessed, and the
// reconcile logs that it swept nothing rather than reporting an empty sweep as
// success. A reader that returned errors for every call would produce the same
// outcomes with more noise and no more information.
func ProjectReaderImpl(ctx context.Context, cfg *config.Config) port.ProjectReader {
	client := natsImpl(ctx, cfg)
	if client == nil {
		return nil
	}
	slog.InfoContext(ctx, "initializing NATS project reader")
	return nats.NewProjectClient(client)
}

// IndexerPublisherImpl returns the publisher that feeds the Formations queue.
//
// Nil when NATS could not be reached, and the sweep treats that as "do not
// publish" rather than as an error to retry. The consequence is worth being
// explicit about, because it is silent from inside this service: checklists are
// still created and lifecycles still move, but the queue stops being refreshed
// and staff see whatever the index last held. Nothing is lost — the projection is
// derived from Postgres and republished on the first sweep after NATS returns.
//
// Shares the one NATS connection with the project reader, so a deployment that
// can read projects can always publish, and neither can be configured without
// the other.
func IndexerPublisherImpl(ctx context.Context, cfg *config.Config) port.IndexerPublisher {
	client := natsImpl(ctx, cfg)
	if client == nil {
		return nil
	}
	slog.InfoContext(ctx, "initializing NATS indexer publisher")
	return nats.NewIndexerPublisher(client)
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

// Deps are the wired dependencies, kept so everything that needs them uses the
// same instances.
//
// This exists because the mock repositories are per-call values, not singletons:
// FormationRepositoryImpl and its siblings return a fresh in-memory store every
// time they are called. Anything that builds its own set therefore gets stores
// nobody else can see — the failure UnitOfWorkImpl's doc comment describes, and
// the reconcile loop reproduced it by wiring itself independently.
type Deps struct {
	Formations port.FormationRepository
	Items      port.ItemRepository
	Activity   port.ActivityRepository
	Templates  port.TemplateRepository
	UnitOfWork port.UnitOfWork
	Projects   port.ProjectReader
	Indexer    port.IndexerPublisher
}

// New builds the wired service. Returns the dependencies so callers that need
// them share these instances rather than building their own, and a close
// function that releases the Postgres pool when REPOSITORY_SOURCE=postgres
// connected one, and a no-op otherwise.
func New(ctx context.Context, cfg *config.Config) (*usecaseSvc.Service, *Deps, func() error, error) {
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
		// NATS first, and unconditionally: Close drains, so in-flight requests
		// finish rather than being cut off, and it is not gated on the Postgres
		// result — returning early on a pool that failed to close would leak
		// the connection.
		if natsClient != nil {
			natsClient.Close()
		}
		if pgDB != nil {
			return pgDB.Close()
		}
		return nil
	}

	deps := &Deps{
		Formations: formations,
		Items:      items,
		Activity:   activity,
		Templates:  templates,
		UnitOfWork: uow,
		Projects:   projects,
		Indexer:    IndexerPublisherImpl(ctx, cfg),
	}

	slog.InfoContext(ctx, "service dependencies wired")
	return svc, deps, closeFn, nil
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
//
// It takes the already-wired deps rather than building its own. In mock mode
// building its own meant writing checklists into stores no request could read.
func StartReconcile(ctx context.Context, cfg *config.Config, deps *Deps) bool {
	if deps.Projects == nil {
		slog.WarnContext(ctx, "reconcile loop not started: nothing can list forming projects yet, "+
			"so no checklist is created automatically. Use formation-cli expand in the meantime")
		return false
	}

	reconciler := usecaseSvc.NewReconciler(
		deps.Projects,
		deps.Formations,
		usecaseSvc.NewExpander(usecaseSvc.NewTemplateSelector(deps.Templates), deps.UnitOfWork, deps.Projects),
		usecaseSvc.NewLifecycler(deps.Formations),
		// The projector may hold a nil publisher, and the sweep still runs: the
		// queue goes stale while checklists are still created and lifecycles
		// still move. Publishing is the one part of the sweep whose failure costs
		// only freshness, so it is the one part allowed to be absent.
		usecaseSvc.NewProjector(deps.Formations, deps.Items, deps.Projects, deps.Indexer),
		// The platform checker resolves nothing yet — no owning service answers a
		// project-scoped existence lookup, so its registry is empty and every
		// platform row is reported unanswerable. Wired regardless, so the count is
		// real and the first lookup is a registry entry rather than a search for
		// where the check was supposed to run.
		usecaseSvc.NewPlatformChecker(deps.UnitOfWork),
		// Configured, not hardcoded. The interval is the worst-case delay before
		// a project that entered formation gets its checklist, so it is the one
		// knob worth turning during an incident — and it previously parsed from
		// the environment into a field nothing read.
		cfg.ReconcileInterval,
	)

	go reconciler.Run(ctx)
	return true
}
