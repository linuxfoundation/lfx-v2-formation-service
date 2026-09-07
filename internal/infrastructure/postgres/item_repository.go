// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// ItemRepo persists checklist items with per-row optimistic locking.
type ItemRepo struct {
	db bun.IDB
}

// NewItemRepo wires an item repository over the given handle.
func NewItemRepo(db bun.IDB) *ItemRepo {
	return &ItemRepo{db: db}
}

// InsertMany expands a checklist. It relies on UNIQUE (formation_uid,
// item_key) and ignores conflicts, so re-running expansion or an upgrade job
// against a formation that already has some items is a no-op for those rows
// rather than an error.
func (r *ItemRepo) InsertMany(ctx context.Context, items []*model.Item) error {
	if len(items) == 0 {
		return nil
	}
	for _, item := range items {
		if item.Revision == 0 {
			item.Revision = 1
		}
		if item.Status == "" {
			item.Status = model.StatusNotStarted
		}
	}

	_, err := r.db.NewInsert().
		Model(&items).
		On("CONFLICT (formation_uid, item_key) DO NOTHING").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("insert items: %w", err)
	}
	return nil
}

// ListByFormation returns every item, ordered the way the checklist screen
// renders sections.
func (r *ItemRepo) ListByFormation(ctx context.Context, formationUID uuid.UUID) ([]*model.Item, error) {
	var items []*model.Item
	err := r.db.NewSelect().
		Model(&items).
		Where("formation_uid = ?", formationUID).
		Order("section_key ASC", "position ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("list items: %w", err)
	}
	return items, nil
}

// Get fetches a single item.
func (r *ItemRepo) Get(ctx context.Context, uid uuid.UUID) (*model.Item, error) {
	item := &model.Item{}
	err := r.db.NewSelect().
		Model(item).
		Where("uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("select item: %w", err)
	}
	return item, nil
}

// Update applies patch under the optimistic lock. Zero rows affected means
// the caller's revision was stale — WHERE revision = $2 is the whole
// mechanism, per row rather than per formation, so concurrent owners editing
// different items never contend on one ETag.
func (r *ItemRepo) Update(ctx context.Context, uid uuid.UUID, revision int64, patch port.ItemPatch) (*model.Item, error) {
	item := &model.Item{}
	q := r.db.NewUpdate().
		Model(item).
		Set("revision = revision + 1").
		Set("updated_at = now()").
		Where("uid = ?", uid).
		Where("revision = ?", revision).
		Returning("*")

	if patch.Status != nil {
		q = q.Set("status = ?", *patch.Status)
	}
	if patch.Assignee != nil {
		q = q.Set("assignee = ?", *patch.Assignee)
	}
	if patch.Note != nil {
		q = q.Set("note = ?", *patch.Note)
	}
	if patch.SkipReason != nil {
		q = q.Set("skip_reason = ?", *patch.SkipReason)
	}
	if patch.EvidenceLink != nil {
		q = q.Set("evidence_link = ?", *patch.EvidenceLink)
	}
	if patch.DueDate != nil {
		q = q.Set("due_date = ?", *patch.DueDate)
	}
	if patch.ResolvedRef != nil {
		q = q.Set("resolved_ref = ?", patch.ResolvedRef)
	}
	if patch.SubItems != nil {
		q = q.Set("sub_items = ?", *patch.SubItems)
	}

	res, err := q.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("update item: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		// affected==0 means either uid doesn't exist or revision was
		// stale. Disambiguate: a client told 412 "re-read and retry"
		// for an unknown uid would 404 on the re-read and retry forever.
		exists, err := r.db.NewSelect().Model((*model.Item)(nil)).Where("uid = ?", uid).Exists(ctx)
		if err != nil {
			return nil, fmt.Errorf("probe item existence: %w", err)
		}
		if !exists {
			return nil, domain.ErrNotFound
		}
		return nil, domain.ErrVersionMismatch
	}
	return item, nil
}

// StatusCounts returns one count per status. Six-way, including skipped as
// its own bucket, never folded into done.
func (r *ItemRepo) StatusCounts(ctx context.Context, formationUID uuid.UUID) (map[model.ItemStatus]int, error) {
	var rows []struct {
		Status model.ItemStatus `bun:"status"`
		Count  int              `bun:"count"`
	}
	err := r.db.NewSelect().
		Model((*model.Item)(nil)).
		Column("status").
		ColumnExpr("count(*) AS count").
		Where("formation_uid = ?", formationUID).
		GroupExpr("status").
		Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("status counts: %w", err)
	}

	counts := make(map[model.ItemStatus]int, len(model.AllItemStatuses))
	for _, s := range model.AllItemStatuses {
		counts[s] = 0
	}
	for _, row := range rows {
		counts[row.Status] = row.Count
	}
	return counts, nil
}

// GateSummary reports how many gating items exist and how many are not yet
// done. Readiness needs both counts: a formation with zero gating items must
// read as not ready, not vacuously ready.
func (r *ItemRepo) GateSummary(ctx context.Context, formationUID uuid.UUID) (total int, outstanding int, err error) {
	var row struct {
		Total       int `bun:"total"`
		Outstanding int `bun:"outstanding"`
	}
	err = r.db.NewSelect().
		Model((*model.Item)(nil)).
		ColumnExpr("count(*) FILTER (WHERE gate) AS total").
		ColumnExpr("count(*) FILTER (WHERE gate AND status <> ?) AS outstanding", model.StatusDone).
		Where("formation_uid = ?", formationUID).
		Scan(ctx, &row)
	if err != nil {
		return 0, 0, fmt.Errorf("gate summary: %w", err)
	}
	return row.Total, row.Outstanding, nil
}
