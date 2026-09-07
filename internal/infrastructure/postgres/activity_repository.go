// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/google/uuid"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// defaultActivityLimit and maxActivityLimit bound the feed page size.
const (
	defaultActivityLimit = 20
	maxActivityLimit     = 100
)

// ActivityRepo appends to and reads the activity feed. There is deliberately
// no update or delete method: the feed is append-only.
type ActivityRepo struct {
	db bun.IDB
}

// NewActivityRepo wires an activity repository over the given handle.
func NewActivityRepo(db bun.IDB) *ActivityRepo {
	return &ActivityRepo{db: db}
}

// Append inserts one entry. Callers write this in the same transaction as the
// change it records, via UnitOfWork, so a failure between the two persists
// neither.
func (r *ActivityRepo) Append(ctx context.Context, e *model.ActivityEntry) error {
	if _, err := r.db.NewInsert().Model(e).Exec(ctx); err != nil {
		return fmt.Errorf("append activity: %w", err)
	}
	return nil
}

// List returns entries newest first. ULID primary keys are time-ordered, so
// paging is a plain "less than the last cursor" predicate on an indexed
// column rather than a separate offset or timestamp scheme.
func (r *ActivityRepo) List(ctx context.Context, formationUID uuid.UUID, cursor string, limit int) ([]*model.ActivityEntry, string, error) {
	if limit <= 0 {
		limit = defaultActivityLimit
	}
	if limit > maxActivityLimit {
		limit = maxActivityLimit
	}

	var rows []*model.ActivityEntry
	q := r.db.NewSelect().
		Model(&rows).
		Where("formation_uid = ?", formationUID).
		OrderExpr("ulid DESC").
		Limit(limit + 1)
	if cursor != "" {
		q = q.Where("ulid < ?", cursor)
	}

	if err := q.Scan(ctx); err != nil {
		return nil, "", fmt.Errorf("list activity: %w", err)
	}

	nextCursor := ""
	if len(rows) > limit {
		nextCursor = rows[limit-1].ULID
		rows = rows[:limit]
	}
	return rows, nextCursor, nil
}
