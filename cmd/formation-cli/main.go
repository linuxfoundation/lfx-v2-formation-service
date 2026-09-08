// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The LFX V2 Formation Service operator CLI.
//
// Template management has no API routes in the first slice, so publishing the
// seeded checklist template is an operator job run as a Kubernetes Job against
// the same database and the same embedded schema as the API. Keeping it in this
// repository rather than in a migration tool is what lets it share the domain
// model, so the content it publishes cannot drift from the shape the service
// reads back.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/log"
)

// Build-time variables set via ldflags in the Makefile.
var (
	version   = "dev"
	buildTime = "unknown"
	gitCommit = "unknown"
)

func init() {
	log.InitStructureLogConfig()
}

const usage = `formation-cli — operator commands for the LFX V2 Formation Service

Usage:
  formation-cli seed              Publish the seeded project formation template
  formation-cli expand <project>  Create a project's checklist from the published
                                  template. Idempotent. The service reconciles
                                  this automatically; use this to create one
                                  ahead of the next sweep.
  formation-cli upgrade [project] Add missing items to existing checklists.
                                  With no argument, covers every checklist.
                                  Adds only; never removes an item or resets a status.
  formation-cli validate          Check the embedded template content, touching no database
  formation-cli version           Print build information
`

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if err := run(context.Background(), flag.Arg(0)); err != nil {
		slog.With(log.ErrKey, err).Error("command failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, command string) error {
	switch command {
	case "seed":
		return seedCommand(ctx)
	case "expand":
		return expandCommand(ctx, flag.Arg(1))
	case "upgrade":
		return upgradeCommand(ctx, flag.Arg(1))
	case "validate":
		// Deliberately reachable with no database configured: the content
		// is the part a reviewer changes, so checking it must not require
		// credentials.
		if _, err := loadSeedSections(); err != nil {
			return err
		}
		slog.InfoContext(ctx, "template content is valid")
		return nil
	case "version":
		fmt.Printf("version=%s build_time=%s git_commit=%s\n", version, buildTime, gitCommit)
		return nil
	case "":
		flag.Usage()
		return fmt.Errorf("no command given")
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

// seedCommand connects, then delegates. The connection is opened here rather
// than in runSeed so the seeding logic stays testable against the mock
// repository.
func seedCommand(ctx context.Context) error {
	return withDatabase(ctx, func(db *postgres.DB) error {
		return runSeed(ctx, postgres.NewTemplateRepo(db.Bun))
	})
}

// expandCommand creates one project's checklist from the published template.
//
// A nil ProjectReader is passed deliberately: no transport can supply the
// announcement date yet, so due rules resolve to no date. Expansion degrades
// rather than refusing, which is the same handling the API path takes.
func expandCommand(ctx context.Context, projectUID string) error {
	if projectUID == "" {
		return fmt.Errorf("expand needs a project UID")
	}

	return withDatabase(ctx, func(db *postgres.DB) error {
		expander := service.NewExpander(
			service.NewTemplateSelector(postgres.NewTemplateRepo(db.Bun)),
			postgres.NewUnitOfWork(db.Bun),
			nil,
		)

		created, err := expander.ExpandFor(ctx, projectUID)
		if err != nil {
			return err
		}
		slog.InfoContext(ctx, "expand finished", "project_uid", projectUID, "created", created)
		return nil
	})
}

// upgradeCommand brings existing checklists onto the published template. An
// empty projectUID means every checklist.
func upgradeCommand(ctx context.Context, projectUID string) error {
	return withDatabase(ctx, func(db *postgres.DB) error {
		upgrader := service.NewUpgrader(
			service.NewTemplateSelector(postgres.NewTemplateRepo(db.Bun)),
			postgres.NewUnitOfWork(db.Bun),
			nil,
		)

		if projectUID != "" {
			report, err := upgrader.UpgradeFor(ctx, projectUID)
			if err != nil {
				return err
			}
			slog.InfoContext(ctx, "upgrade finished",
				"project_uid", report.ProjectUID, "added", len(report.AddedKeys))
			return nil
		}

		reports, err := upgrader.UpgradeAll(ctx)
		// Reported either way: UpgradeAll returns what succeeded alongside the
		// error naming how many did not, and an operator needs both.
		added := 0
		for _, report := range reports {
			added += len(report.AddedKeys)
		}
		slog.InfoContext(ctx, "upgrade finished",
			"checklists", len(reports), "items_added", added)
		return err
	})
}

// withDatabase opens the pool, runs fn, and closes it. postgres.Connect applies
// the embedded schema, so the CLI needs no separate migration step and cannot
// run against a database the API has not prepared.
func withDatabase(ctx context.Context, fn func(*postgres.DB) error) error {
	cfg := config.LoadConfig()

	slog.InfoContext(ctx, "connecting to postgres", "database", cfg.Database.Redacted())
	db, err := postgres.Connect(ctx, cfg.Database.DSN())
	if err != nil {
		return fmt.Errorf("connecting to postgres: %w", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			slog.With(log.ErrKey, closeErr).WarnContext(ctx, "closing postgres pool")
		}
	}()

	return fn(db)
}
