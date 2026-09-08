// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package postgres provides the transactional store for formations, their
// items and the activity feed.
package postgres

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// advisoryLockKey is an arbitrary 64-bit constant used with
// pg_advisory_xact_lock to serialize concurrent pods during bootstrap.
const advisoryLockKey int64 = 0x464F_524D_4154_494F // "FORMATIO"

// schemaLockTimeout bounds how long a pod waits for the advisory lock.
const schemaLockTimeout = "60s"

// ApplySchema runs the embedded schema.sql in a single transaction, gated by a
// Postgres transaction-scoped advisory lock so concurrent pod startups cannot
// race on CREATE statements. The schema is idempotent, so applying it twice is
// a no-op.
func ApplySchema(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin schema tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// pg_advisory_xact_lock blocks indefinitely by default, so without a
	// bound a hung peer pod would stall every later rollout rather than
	// failing this one. SET LOCAL scopes the timeout to this transaction.
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+schemaLockTimeout+"'"); err != nil {
		return fmt.Errorf("set statement timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}

	slog.InfoContext(ctx, "applying database schema")
	if _, err := tx.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit schema tx: %w", err)
	}
	slog.InfoContext(ctx, "database schema applied")
	return nil
}
