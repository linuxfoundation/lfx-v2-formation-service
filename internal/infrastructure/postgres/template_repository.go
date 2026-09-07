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
)

// TemplateRepo persists and selects checklist templates.
type TemplateRepo struct {
	db bun.IDB
}

// NewTemplateRepo wires a template repository over the given handle.
func NewTemplateRepo(db bun.IDB) *TemplateRepo {
	return &TemplateRepo{db: db}
}

// ListPublished returns published templates ordered by priority, so
// selection is a first-match walk over the result (lower priority wins).
func (r *TemplateRepo) ListPublished(ctx context.Context) ([]*model.Template, error) {
	var templates []*model.Template
	err := r.db.NewSelect().
		Model(&templates).
		Where("state = ?", model.TemplatePublished).
		Order("priority ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("list published templates: %w", err)
	}
	return templates, nil
}

// Get fetches a single template by uid.
func (r *TemplateRepo) Get(ctx context.Context, uid uuid.UUID) (*model.Template, error) {
	t := &model.Template{}
	err := r.db.NewSelect().
		Model(t).
		Where("uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("select template: %w", err)
	}
	return t, nil
}

// Upsert seeds or replaces a template version, keyed on UNIQUE (name,
// version), so re-running the seed job is a no-op once a version exists
// unchanged and an update once its content changes.
func (r *TemplateRepo) Upsert(ctx context.Context, t *model.Template) (*model.Template, error) {
	t.ApplyUpsertDefaults()

	_, err := r.db.NewInsert().
		Model(t).
		On("CONFLICT (name, version) DO UPDATE").
		Set("state = EXCLUDED.state").
		Set("priority = EXCLUDED.priority").
		Set("match = EXCLUDED.match").
		Set("sections = EXCLUDED.sections").
		Set("author = EXCLUDED.author").
		Set("updated_at = now()").
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert template: %w", err)
	}
	return t, nil
}
