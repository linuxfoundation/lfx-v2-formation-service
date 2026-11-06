// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/service"
)

// parseSyncRequiresWriterArgs reads the command's own flags. Parsed before the
// database is opened, so a mistyped flag fails without connecting.
func parseSyncRequiresWriterArgs(args []string) (apply bool, err error) {
	fs := flag.NewFlagSet("sync-requires-writer", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&apply, "apply", false, "write the corrections; without it the command only reports")
	if err := fs.Parse(args); err != nil {
		return false, fmt.Errorf("sync-requires-writer: %w", err)
	}
	if fs.NArg() != 0 {
		return false, fmt.Errorf("sync-requires-writer takes no arguments, got %q", fs.Args())
	}
	return apply, nil
}

// syncRequiresWriterCommand corrects requires_writer on live checklists
// expanded from any version of the published template's name to that
// template's values. A dry run unless --apply is given.
func syncRequiresWriterCommand(ctx context.Context, args []string) error {
	apply, err := parseSyncRequiresWriterArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "usage: formation-cli sync-requires-writer [--apply]")
		return nil
	}
	if err != nil {
		return err
	}
	return withDatabase(ctx, func(db *postgres.DB) error {
		syncer := service.NewRequiresWriterSync(
			service.NewTemplateSelector(postgres.NewTemplateRepo(db.Bun)),
			postgres.NewUnitOfWork(db.Bun),
		)
		report, err := syncer.Run(ctx, apply)
		if err != nil {
			return err
		}
		logRequiresWriterReport(ctx, report)
		return nil
	})
}

// logRequiresWriterReport names keys and counts only, never a project or a
// checklist, so the output can be pasted into a ticket as it stands.
func logRequiresWriterReport(ctx context.Context, report *service.RequiresWriterReport) {
	mode := "dry-run"
	if report.Applied {
		mode = "apply"
	}
	for _, k := range report.Keys {
		slog.InfoContext(ctx, "requires_writer",
			"mode", mode,
			"template", report.TemplateName,
			"template_version", report.TemplateVersion,
			"item_key", k.ItemKey,
			"want", k.Want,
			"changed", k.Changed,
			"already_correct", k.AlreadyCorrect,
		)
	}
	notInTemplate := 0
	for _, s := range report.Skipped {
		notInTemplate += s.Count
		slog.WarnContext(ctx, "requires_writer skipped",
			"mode", mode,
			"item_key", s.ItemKey,
			"count", s.Count,
			"reason", s.Reason,
		)
	}
	slog.InfoContext(ctx, "requires_writer summary",
		"mode", mode,
		"template", report.TemplateName,
		"template_version", report.TemplateVersion,
		"changed", report.Changed(),
		"already_correct", report.AlreadyCorrect(),
		"skipped_not_in_template", notInTemplate,
		"skipped_finished", report.Finished,
	)
}
