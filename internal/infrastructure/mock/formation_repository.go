// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package mock provides hand-written port doubles, one per port, selected by
// REPOSITORY_SOURCE=mock in cmd/formation-api/service/providers.go. No
// mock-generation tooling is used, matching lfx-v2-committee-service's
// internal/infrastructure/mock/ convention.
package mock

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// FormationRepository is an in-memory port.FormationRepository double, keyed
// on project UID the same way the real uniqueness constraint is.
type FormationRepository struct {
	mu        sync.Mutex
	byUID     map[uuid.UUID]*model.Formation
	byProject map[string]uuid.UUID
}

// NewFormationRepository constructs an empty double.
func NewFormationRepository() *FormationRepository {
	return &FormationRepository{
		byUID:     make(map[uuid.UUID]*model.Formation),
		byProject: make(map[string]uuid.UUID),
	}
}

// Create inserts a formation, returning domain.ErrAlreadyExists when the
// project already has one — callers treat that as success, per the port's
// contract.
func (r *FormationRepository) Create(_ context.Context, f *model.Formation) (*model.Formation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.byProject[f.ProjectUID]; exists {
		return nil, domain.ErrAlreadyExists
	}

	clone := *f
	if clone.UID == uuid.Nil {
		clone.UID = uuid.New()
	}
	if clone.Lifecycle == "" {
		clone.Lifecycle = model.LifecycleLive
	}
	clone.Revision = 1
	r.byUID[clone.UID] = &clone
	r.byProject[clone.ProjectUID] = clone.UID

	out := clone
	return &out, nil
}

// GetByProject returns the formation for a project, or domain.ErrNotFound.
func (r *FormationRepository) GetByProject(_ context.Context, projectUID string) (*model.Formation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	uid, ok := r.byProject[projectUID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	out := *r.byUID[uid]
	return &out, nil
}

// UpdateLifecycle moves the formation's lifecycle, refusing the write when
// revision is stale (domain.ErrVersionMismatch).
func (r *FormationRepository) UpdateLifecycle(_ context.Context, uid uuid.UUID, lifecycle model.Lifecycle, revision int64) (*model.Formation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, ok := r.byUID[uid]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if f.Revision != revision {
		return nil, domain.ErrVersionMismatch
	}

	f.Lifecycle = lifecycle
	f.Revision++
	out := *f
	return &out, nil
}

// ListProjectUIDs returns every project that already has a formation.
func (r *FormationRepository) ListProjectUIDs(_ context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.byProject))
	for projectUID := range r.byProject {
		out = append(out, projectUID)
	}
	return out, nil
}
