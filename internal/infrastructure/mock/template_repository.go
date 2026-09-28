// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// TemplateRepository is an in-memory port.TemplateRepository double.
type TemplateRepository struct {
	Recorder
	mu        sync.Mutex
	templates map[uuid.UUID]*model.Template
}

// NewTemplateRepository constructs an empty double.
func NewTemplateRepository() *TemplateRepository {
	return &TemplateRepository{templates: make(map[uuid.UUID]*model.Template)}
}

// ListPublished returns published templates ordered by priority.
func (r *TemplateRepository) ListPublished(_ context.Context) ([]*model.Template, error) {
	r.record("templates.ListPublished")
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
	r.record("templates.Get")
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.templates[uid]
	if !ok {
		return nil, domain.ErrNotFound
	}
	out := *t
	return &out, nil
}

// List returns all templates regardless of state, ordered by name then version.
func (r *TemplateRepository) List(_ context.Context) ([]*model.Template, error) {
	r.record("templates.List")
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*model.Template, 0, len(r.templates))
	for _, t := range r.templates {
		clone := *t
		out = append(out, &clone)
	}
	// Name then version ascending, mirroring the Postgres query.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j], out[j-1]
			if a.Name < b.Name || (a.Name == b.Name && a.Version < b.Version) {
				out[j], out[j-1] = out[j-1], out[j]
			} else {
				break
			}
		}
	}
	return out, nil
}

// Create inserts a new draft template.
func (r *TemplateRepository) Create(_ context.Context, t *model.Template) (*model.Template, error) {
	r.record("templates.Create")
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, existing := range r.templates {
		if existing.Name == t.Name && existing.Version == t.Version {
			return nil, fmt.Errorf("%w: template %s v%d already exists", domain.ErrConflict, t.Name, t.Version)
		}
	}

	clone := *t
	clone.UID = uuid.New()
	clone.State = model.TemplateDraft
	if clone.Sections == nil {
		clone.Sections = []model.TemplateSection{}
	}
	r.templates[clone.UID] = &clone
	out := clone
	return &out, nil
}

// Update applies mutable fields to a draft template.
func (r *TemplateRepository) Update(_ context.Context, uid uuid.UUID, patch port.TemplatePatch) (*model.Template, error) {
	r.record("templates.Update")
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.templates[uid]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if t.State != model.TemplateDraft {
		return nil, fmt.Errorf("%w: only draft templates may be updated", domain.ErrConflict)
	}

	clone := *t
	if patch.Priority != nil {
		clone.Priority = *patch.Priority
	}
	if patch.Match != nil {
		clone.Match = *patch.Match
	}
	if patch.Sections != nil {
		clone.Sections = *patch.Sections
	}
	if patch.Author != nil {
		clone.Author = *patch.Author
	}
	r.templates[uid] = &clone
	out := clone
	return &out, nil
}

// Publish transitions a draft template to published.
func (r *TemplateRepository) Publish(_ context.Context, uid uuid.UUID) (*model.Template, error) {
	r.record("templates.Publish")
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.templates[uid]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if t.State != model.TemplateDraft {
		return nil, fmt.Errorf("%w: only draft templates may be published", domain.ErrConflict)
	}

	clone := *t
	clone.State = model.TemplatePublished
	now := time.Now().UTC()
	clone.PublishedAt = &now
	r.templates[uid] = &clone
	out := clone
	return &out, nil
}

// Archive transitions a template to archived.
func (r *TemplateRepository) Archive(_ context.Context, uid uuid.UUID) (*model.Template, error) {
	r.record("templates.Archive")
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.templates[uid]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if t.State == model.TemplateArchived {
		return nil, fmt.Errorf("%w: template is already archived", domain.ErrConflict)
	}

	clone := *t
	clone.State = model.TemplateArchived
	r.templates[uid] = &clone
	out := clone
	return &out, nil
}

// Upsert seeds a template, keyed on name and version so re-running the seed job
// is a no-op. Editing the content of an already-published version is refused
// with domain.ErrConflict, mirroring the repository — a double that allowed it
// would let the seed command's guard pass in tests and fail against Postgres.
func (r *TemplateRepository) Upsert(_ context.Context, t *model.Template) (*model.Template, error) {
	r.record("templates.Upsert")
	r.mu.Lock()
	defer r.mu.Unlock()

	t.ApplyUpsertDefaults()

	for uid, existing := range r.templates {
		if existing.Name == t.Name && existing.Version == t.Version {
			// Ever published, not published right now, matching the repository:
			// the guarantee covers a version some checklist already expanded
			// from, and a state that has moved on since does not release it.
			everPublished := existing.State == model.TemplatePublished || existing.PublishedAt != nil
			if everPublished && !sameSections(existing.Sections, t.Sections) {
				return nil, fmt.Errorf("%w: %s v%d has been published and its content cannot be edited in place; "+
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
