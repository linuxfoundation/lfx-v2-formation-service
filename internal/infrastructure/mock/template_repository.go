// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// TemplateRepository is an in-memory port.TemplateRepository double.
type TemplateRepository struct {
	mu        sync.Mutex
	templates map[uuid.UUID]*model.Template
}

// NewTemplateRepository constructs an empty double.
func NewTemplateRepository() *TemplateRepository {
	return &TemplateRepository{templates: make(map[uuid.UUID]*model.Template)}
}

// ListPublished returns published templates ordered by priority.
func (r *TemplateRepository) ListPublished(_ context.Context) ([]*model.Template, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*model.Template, 0)
	for _, t := range r.templates {
		if t.State == model.TemplatePublished {
			clone := *t
			out = append(out, &clone)
		}
	}
	// Priority first, then newest version — the same order the Postgres query
	// uses, because a double that ordered candidates differently would let a
	// selection bug pass here and fail in production. Simple insertion sort;
	// the candidate lists this double serves in tests are small.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && lessByPriorityThenVersion(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// lessByPriorityThenVersion mirrors the repository's ORDER BY: lower priority
// first, and within one priority the newer version first.
func lessByPriorityThenVersion(a, b *model.Template) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return a.Version > b.Version
}

// sameSections compares content the way the repository does, by its JSON form.
func sameSections(a, b []model.TemplateSection) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

// Get returns one template by UID, or domain.ErrNotFound.
func (r *TemplateRepository) Get(_ context.Context, uid uuid.UUID) (*model.Template, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.templates[uid]
	if !ok {
		return nil, domain.ErrNotFound
	}
	out := *t
	return &out, nil
}

// Upsert seeds a template, keyed on name and version so re-running the seed job
// is a no-op. Editing the content of an already-published version is refused
// with domain.ErrConflict, mirroring the repository — a double that allowed it
// would let the seed command's guard pass in tests and fail against Postgres.
func (r *TemplateRepository) Upsert(_ context.Context, t *model.Template) (*model.Template, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t.ApplyUpsertDefaults()

	for uid, existing := range r.templates {
		if existing.Name == t.Name && existing.Version == t.Version {
			if existing.State == model.TemplatePublished && !sameSections(existing.Sections, t.Sections) {
				return nil, fmt.Errorf("%w: %s v%d is published and its content cannot be edited in place; "+
					"bump the version instead so existing checklists keep pinning what they expanded from",
					domain.ErrConflict, t.Name, t.Version)
			}
			clone := *t
			clone.UID = uid
			// COALESCE(existing, incoming), matching the repository's ON
			// CONFLICT clause: the first publication time survives a re-seed,
			// and a draft re-seeded as published picks one up. Overwriting it
			// here would make the double record the latest seed instead, and
			// the guarantee is only implemented in SQL — nothing service-level
			// would catch the difference.
			if existing.PublishedAt != nil {
				clone.PublishedAt = existing.PublishedAt
			}
			r.templates[uid] = &clone
			out := clone
			return &out, nil
		}
	}

	clone := *t
	if clone.UID == uuid.Nil {
		clone.UID = uuid.New()
	}
	r.templates[clone.UID] = &clone
	out := clone
	return &out, nil
}
