// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
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
	// Simple insertion sort by priority; the candidate lists this double
	// serves in tests are small.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Priority < out[j-1].Priority; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
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

// Upsert seeds or replaces a template, keyed on name and version so
// re-running the seed job is a no-op.
func (r *TemplateRepository) Upsert(_ context.Context, t *model.Template) (*model.Template, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t.ApplyUpsertDefaults()

	for uid, existing := range r.templates {
		if existing.Name == t.Name && existing.Version == t.Version {
			clone := *t
			clone.UID = uid
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
