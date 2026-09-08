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
//
// The returned keys are the rows this statement actually wrote. DO NOTHING
// returns nothing for a suppressed row, so the list is exactly what was added
// and a caller recording the change describes what happened rather than what it
// hoped would happen.
func (r *ItemRepo) InsertMany(ctx context.Context, items []*model.Item) ([]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	for _, item := range items {
		item.ApplyInsertDefaults()
	}

	// Scanned into its own slice rather than back into the models. RETURNING
	// yields a row per inserted item and nothing for a suppressed one, so with
	// any conflict the result no longer lines up positionally with the input —
	// letting Bun scan it back into the models would attach one item's generated
	// UID to another.
	var returned []*model.Item
	if _, err := r.db.NewInsert().
		Model(&items).
		On("CONFLICT (formation_uid, item_key) DO NOTHING").
		Returning("*").
		Exec(ctx, &returned); err != nil {
		return nil, fmt.Errorf("insert items: %w", err)
	}

	// Matched by key, which is what the conflict target makes unique, so each
	// caller's item still comes back carrying what the database generated for it.
	byKey := make(map[itemIdentity]*model.Item, len(returned))
	inserted := make([]string, 0, len(returned))
	for _, row := range returned {
		byKey[itemIdentity{row.FormationUID, row.ItemKey}] = row
		inserted = append(inserted, row.ItemKey)
	}
	for _, item := range items {
		if row, ok := byKey[itemIdentity{item.FormationUID, item.ItemKey}]; ok {
			*item = *row
		}
	}
	return inserted, nil
}

// itemIdentity is the pair UNIQUE (formation_uid, item_key) covers.
type itemIdentity struct {
	formationUID uuid.UUID
	itemKey      string
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

// GetByKey fetches the item at item_key within a formation.
func (r *ItemRepo) GetByKey(ctx context.Context, formationUID uuid.UUID, itemKey string) (*model.Item, error) {
	item := &model.Item{}
	err := r.db.NewSelect().
		Model(item).
		Where("formation_uid = ?", formationUID).
		Where("item_key = ?", itemKey).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("select item by key: %w", err)
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
		if *patch.Assignee == "" {
			// Same clear signal as due_date, and NULL rather than '' for the
			// same reason it matters here: formation_items_assignee_idx is
			// partial on assignee IS NOT NULL, so an unassigned row stored as
			// '' stays in the index of assigned work.
			q = q.Set("assignee = NULL")
		} else {
			q = q.Set("assignee = ?", *patch.Assignee)
		}
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
		if *patch.DueDate == "" {
			// An empty string is the clear signal (see item_mutator.go's
			// buildItemPatch): due_date is a DATE column, which rejects ''
			// outright, so clearing has to be a real NULL rather than the
			// raw string passed straight through like every other field.
			q = q.Set("due_date = NULL")
		} else {
			q = q.Set("due_date = ?", *patch.DueDate)
		}
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
