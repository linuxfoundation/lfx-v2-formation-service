// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"os"
	"strconv"
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

// The repository row is made manual on rows that already exist, and nothing
// else about them changes.
//
// The delicate part is not the update, it is everything the update must not
// touch. These rows belong to live checklists that people are working through,
// so the migration changes who is expected to answer the row and leaves the
// answer alone — a row somebody marked done or blocked keeps that status, its
// note, and its revision. A migration that reset statuses would silently undo
// real work on every checklist in flight, and it would look like a successful
// deploy.
func TestApplySchemaMakesTheRepositoryRowManualWithoutTouchingAnyStatus(t *testing.T) {
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
		 VALUES ('migration-test', 1, 'published', 100, 'always', '[]'::jsonb)
		 RETURNING uid`,
	).Scan(&templateUID); err != nil {
		t.Fatalf("seeding the template: %v", err)
	}
	// One checklist per status, because an item key is unique within a
	// checklist. That is closer to production anyway: these rows are spread
	// across many projects in whatever state each of them reached, and the
	// migration has to leave every one of those states alone.
	statuses := []string{"not_started", "in_progress", "blocked", "done", "skipped"}
	var firstFormationUID string
	for i, status := range statuses {
		var formationUID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO formations (project_uid, template_uid, template_version, lifecycle)
			 VALUES ($1, $2, 1, 'live') RETURNING uid`,
			"project-with-old-rows-"+status, templateUID,
		).Scan(&formationUID); err != nil {
			t.Fatalf("seeding the formation for %s: %v", status, err)
		}
		if i == 0 {
			firstFormationUID = formationUID
		}
		// A skipped row carries a reason or the table refuses it, which is
		// itself worth exercising here: the migration must not disturb that
		// pairing either.
		var skipReason *string
		if status == "skipped" {
			reason := "the project brought its own repositories"
			skipReason = &reason
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO formation_items
			   (formation_uid, item_key, section_key, title, position, status, status_source,
			    platform_check, note, skip_reason)
			 VALUES ($1, 'repositories_github_owner', 'community_and_launch', 'Repositories and GitHub owner',
			         0, $2, 'platform', $3::jsonb, 'a person wrote this', $4)`,
			formationUID, status, `{"resource_type":"repository","min_count":1}`, skipReason,
		); err != nil {
			t.Fatalf("seeding a repository row at %s: %v", status, err)
		}
	}

	// A platform row that is not the repository row, to prove the migration is
	// keyed on the item rather than sweeping every platform-sourced row.
	if _, err := pool.Exec(ctx,
		`INSERT INTO formation_items
		   (formation_uid, item_key, section_key, title, position, status, status_source, platform_check)
		 VALUES ($1, 'tsc_kickoff', 'community_and_launch', 'Charter the TSC', 99,
		         'not_started', 'platform', $2::jsonb)`,
		firstFormationUID, `{"resource_type":"committee","min_count":1}`,
	); err != nil {
		t.Fatalf("seeding the committee row: %v", err)
	}

	before := statusDistribution(ctx, t, pool)

	if err := ApplySchema(ctx, pool); err != nil {
		t.Fatalf("applying the migration: %v", err)
	}

	var stillPlatform int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM formation_items
		  WHERE item_key = 'repositories_github_owner'
		    AND (status_source <> 'manual' OR platform_check IS NOT NULL)`,
	).Scan(&stillPlatform); err != nil {
		t.Fatalf("counting unmigrated rows: %v", err)
	}
	if stillPlatform != 0 {
		t.Errorf("%d repository rows are still platform-sourced, want 0", stillPlatform)
	}

	// The committee row is untouched: it is answerable, and the migration has
	// no business in it.
	var committeeSource string
	if err := pool.QueryRow(ctx,
		`SELECT status_source FROM formation_items WHERE item_key = 'tsc_kickoff'`,
	).Scan(&committeeSource); err != nil {
		t.Fatalf("reading the committee row: %v", err)
	}
	if committeeSource != "platform" {
		t.Errorf("committee row status_source = %q, want it left as platform", committeeSource)
	}

	if after := statusDistribution(ctx, t, pool); after != before {
		t.Errorf("status distribution changed across the migration:\nbefore %s\nafter  %s\n"+
			"this migration changes who answers the row, never what the answer is", before, after)
	}

	var notes int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM formation_items
		  WHERE item_key = 'repositories_github_owner' AND note = 'a person wrote this'`,
	).Scan(&notes); err != nil {
		t.Fatalf("counting notes: %v", err)
	}
	if notes != len(statuses) {
		t.Errorf("%d rows kept their note, want %d", notes, len(statuses))
	}

	// Applied twice is applied once. The schema runs on every pod start, so a
	// migration that was not idempotent would fire on every deploy.
	if err := ApplySchema(ctx, pool); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if again := statusDistribution(ctx, t, pool); again != before {
		t.Errorf("re-applying the schema changed the status distribution: %s", again)
	}
}

// statusDistribution renders the count of rows per status as a stable string,
// so a test can compare the whole distribution before and after rather than
// picking statuses to check one at a time.
func statusDistribution(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT status, count(*) FROM formation_items GROUP BY status ORDER BY status`)
	if err != nil {
		t.Fatalf("reading the status distribution: %v", err)
	}
	defer rows.Close()

	var out strings.Builder
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			t.Fatalf("scanning the status distribution: %v", err)
		}
		out.WriteString(status)
		out.WriteString("=")
		out.WriteString(strconv.Itoa(count))
		out.WriteString(" ")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the status distribution: %v", err)
	}
	return out.String()
}
