// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

// The schema is applied on every pod start, so re-applying it has to be a
// no-op rather than an error. That property is only observable against a real
// database, so these tests are gated on FORMATION_TEST_DATABASE_URL: unset
// (a plain local `go test ./...`) skips.

// testPool connects to the test database, refusing anything that does not look
// like one, since these tests create and drop objects.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("FORMATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FORMATION_TEST_DATABASE_URL not set — skipping Postgres-backed schema tests")
	}

	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	if !strings.HasSuffix(cfg.ConnConfig.Database, "_test") {
		t.Fatalf("refusing schema tests against database %q — FORMATION_TEST_DATABASE_URL must name a dedicated test database with a \"_test\" suffix", cfg.ConnConfig.Database)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testDB connects, applies the schema, truncates every table this service
// owns and returns a ready bun.DB for repository-level tests. Truncating
// up front (rather than relying on each test to clean up after itself) means
// a failed test run never poisons the next one with leftover rows that
// collide on a UNIQUE constraint.
func testDB(t *testing.T) *bun.DB {
	t.Helper()
	ctx := context.Background()
	pool := testPool(t)
	if err := ApplySchema(ctx, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`TRUNCATE formation_activity, formation_items, formations, formation_templates CASCADE`,
	); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}
	db := bun.NewDB(stdlib.OpenDBFromPool(pool), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestApplySchemaIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	if err := ApplySchema(ctx, pool); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := ApplySchema(ctx, pool); err != nil {
		t.Fatalf("second apply must be a no-op, got: %v", err)
	}

	// Every table the service depends on must exist after the applies, and
	// the second apply must not have duplicated any of them.
	for _, table := range []string{
		"formation_templates",
		"formations",
		"formation_items",
		"formation_activity",
	} {
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			  WHERE table_schema = current_schema() AND table_name = $1`,
			table,
		).Scan(&count); err != nil {
			t.Fatalf("checking table %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %s: got %d definitions, want exactly 1", table, count)
		}
	}
}

// The skip-needs-reason constraint is the one piece of business logic the
// schema itself enforces, so a re-apply must leave it in place rather than
// silently dropping it.
func TestSkipRequiresReasonSurvivesReapply(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	if err := ApplySchema(ctx, pool); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := ApplySchema(ctx, pool); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint WHERE conname = 'skip_needs_reason'`,
	).Scan(&count); err != nil {
		t.Fatalf("checking constraint: %v", err)
	}
	if count != 1 {
		t.Errorf("skip_needs_reason: got %d constraints, want exactly 1", count)
	}
}

// The column additions at the end of the file are the whole reason this passes:
// CREATE TABLE IF NOT EXISTS does nothing to a table that exists, so a column
// added to the definition alone never reaches a database that has already been
// created — and every read and write of that table then fails on it.
//
// The previous table shape is reproduced by dropping the column, which is what
// a database created before it looks like. The backfill matters as much as the
// column: the reader takes sections from here alone, so a checklist that
// predates it would otherwise serve an empty sections[].
func TestApplySchemaAddsAndBackfillsSectionsOverAnOlderTable(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	if err := ApplySchema(ctx, pool); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`TRUNCATE formation_activity, formation_items, formations, formation_templates CASCADE`,
	); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	var templateUID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO formation_templates (name, version, state, priority, match, sections)
		 VALUES ('migration-test', 1, 'published', 100, 'always', $1::jsonb)
		 RETURNING uid`,
		`[{"key":"legal_and_entity","title":"Legal and entity","items":[]},
		  {"key":"community_and_launch","title":"Community and launch","items":[]}]`,
	).Scan(&templateUID); err != nil {
		t.Fatalf("seeding the template: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO formations (project_uid, template_uid, template_version, lifecycle)
		 VALUES ('project-before-the-column', $1, 1, 'live')`,
		templateUID,
	); err != nil {
		t.Fatalf("seeding the formation: %v", err)
	}

	// The database as it was before the column existed.
	if _, err := pool.Exec(ctx, `ALTER TABLE formations DROP COLUMN sections`); err != nil {
		t.Fatalf("reproducing the older table shape: %v", err)
	}

	if err := ApplySchema(ctx, pool); err != nil {
		t.Fatalf("applying over the older table shape: %v", err)
	}

	var sections string
	if err := pool.QueryRow(ctx,
		`SELECT sections::text FROM formations WHERE project_uid = 'project-before-the-column'`,
	).Scan(&sections); err != nil {
		t.Fatalf("reading sections after the migration: %v", err)
	}
	for _, want := range []string{"legal_and_entity", "Legal and entity", "community_and_launch"} {
		if !strings.Contains(sections, want) {
			t.Errorf("sections = %s, want it backfilled from the pinned template (missing %q)", sections, want)
		}
	}

	// Re-running must not reset a snapshot an upgrade has since extended, which
	// is what the "only rows that have nothing" guard on the backfill is for.
	if _, err := pool.Exec(ctx,
		`UPDATE formations SET sections = $1::jsonb WHERE project_uid = 'project-before-the-column'`,
		`[{"key":"added_by_an_upgrade","title":"Added by an upgrade"}]`,
	); err != nil {
		t.Fatalf("simulating an extended snapshot: %v", err)
	}
	if err := ApplySchema(ctx, pool); err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT sections::text FROM formations WHERE project_uid = 'project-before-the-column'`,
	).Scan(&sections); err != nil {
		t.Fatalf("re-reading sections: %v", err)
	}
	if !strings.Contains(sections, "added_by_an_upgrade") {
		t.Errorf("sections = %s, want the extended snapshot left alone by a re-apply", sections)
	}
}
