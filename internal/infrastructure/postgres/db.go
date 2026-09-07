// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

// DB bundles the three handles the service needs over one connection pool:
// the pgx pool for schema bootstrap and raw aggregate queries, and the bun DB
// the repositories build statements with. They share the pool, so there is a
// single set of connections to size and close.
type DB struct {
	Pool *pgxpool.Pool
	SQL  *sql.DB
	Bun  *bun.DB
}

// Connect opens the pool from a composed DSN and applies the schema. The DSN
// is passed in already composed so this package never has to know how the
// credentials were sourced.
func Connect(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open pgx pool: %w", err)
	}

	// Fail here rather than at the first query, so a bad credential surfaces
	// as a startup error instead of a request error.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	if err := ApplySchema(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	return &DB{
		Pool: pool,
		SQL:  sqlDB,
		Bun:  bun.NewDB(sqlDB, pgdialect.New()),
	}, nil
}

// Ping reports whether the database is reachable. Used by the readiness probe.
func (d *DB) Ping(ctx context.Context) error {
	if d == nil || d.Pool == nil {
		return fmt.Errorf("database not initialized")
	}
	return d.Pool.Ping(ctx)
}

// Close releases the pool. Closing the stdlib handle first releases the
// connections it borrowed, then the pool itself goes.
func (d *DB) Close() error {
	if d == nil {
		return nil
	}
	if d.SQL != nil {
		if err := d.SQL.Close(); err != nil {
			slog.Warn("closing sql handle", "error", err)
		}
	}
	if d.Pool != nil {
		d.Pool.Close()
	}
	return nil
}
