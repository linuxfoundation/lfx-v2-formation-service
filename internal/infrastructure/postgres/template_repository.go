// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
		// Version breaks the tie priority alone leaves. Publishing v2 does not
		// retire v1, so both sit here at the same priority and priority-only
		// ordering lets the database return them in either order — selection
		// would pick a version by luck, and an upgrade could walk a checklist
		// backwards onto older content.
		Order("version DESC").
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

// Upsert seeds a template version, keyed on UNIQUE (name, version), so
// re-running the seed job is a no-op.
//
// A published version's content is immutable. Re-seeding one with identical
// sections stays a no-op, but changing them is refused with domain.ErrConflict
// and the operator has to bump the version instead. Checklists pin the version
// they expanded from, so editing that version in place would silently change
// what every existing pin refers to — two checklists could claim the same
// version having been built from different content, and the pin would stop
// being evidence of anything.
func (r *TemplateRepo) Upsert(ctx context.Context, t *model.Template) (*model.Template, error) {
	t.ApplyUpsertDefaults()

	if err := r.refuseEditingAPublishedVersion(ctx, t); err != nil {
		return nil, err
	}

	_, err := r.db.NewInsert().
		Model(t).
		On("CONFLICT (name, version) DO UPDATE").
		Set("state = EXCLUDED.state").
		Set("priority = EXCLUDED.priority").
		Set("match = EXCLUDED.match").
		Set("sections = EXCLUDED.sections").
		Set("author = EXCLUDED.author").
		// Carried through only when there is nothing there yet. Both halves
		// matter: leaving it alone entirely would let a draft re-seeded as
		// published keep a NULL published_at beside state = 'published', which
		// reads as "never published"; overwriting it unconditionally would move
		// the timestamp forward on every re-seed, so the column would record
		// the most recent seed rather than the first publication it is there
		// to date.
		// "t" is the model's Bun alias; the unaliased table name is not in scope
		// inside ON CONFLICT DO UPDATE here.
		Set("published_at = COALESCE(t.published_at, EXCLUDED.published_at)").
		Set("updated_at = now()").
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert template: %w", err)
	}
	return t, nil
}

// refuseEditingAPublishedVersion returns domain.ErrConflict when the incoming
// template would change the content of a version that is already published.
//
// This is a read followed by a write rather than one statement, so two seeds
// racing could still both pass the check. That is acceptable here and not worth
// a lock: seeding is an operator command, not a request path, and the failure it
// exists to prevent is an operator editing content and forgetting to bump —
// which this catches on the first attempt.
func (r *TemplateRepo) refuseEditingAPublishedVersion(ctx context.Context, t *model.Template) error {
	existing := &model.Template{}
	err := r.db.NewSelect().
		Model(existing).
		Where("name = ?", t.Name).
		Where("version = ?", t.Version).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // A version nobody has seen yet; nothing to protect.
		}
		return fmt.Errorf("reading the stored template before seeding: %w", err)
	}
	// Anything that has ever been published is protected, not only what is
	// published right now. The guarantee this enforces is about the past — a
	// checklist pinned this version and expanded from its content — so a check
	// on the current state alone is one a caller can step around: seeding the
	// same content with a different state passes here, because only content is
	// compared, and the row it leaves behind is no longer published, so the next
	// seed may rewrite it freely. published_at is the durable half, kept by the
	// COALESCE in Upsert precisely so the first publication survives a re-seed.
	if existing.State != model.TemplatePublished && existing.PublishedAt == nil {
		return nil // Never published, so its content is still open to change.
	}

	same, err := sameSections(existing.Sections, t.Sections)
	if err != nil {
		return err
	}
	if same {
		return nil
	}

	return fmt.Errorf("%w: %s v%d has been published and its content cannot be edited in place; "+
		"bump the version instead so existing checklists keep pinning what they expanded from",
		domain.ErrConflict, t.Name, t.Version)
}

// sameSections compares template content by its stored JSON form, which is the
// same representation the column round-trips, so a reordering the database would
// not preserve cannot read as a change.
func sameSections(a, b []model.TemplateSection) (bool, error) {
	left, err := json.Marshal(a)
	if err != nil {
		return false, fmt.Errorf("comparing stored template content: %w", err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false, fmt.Errorf("comparing incoming template content: %w", err)
	}
	return bytes.Equal(left, right), nil
}
