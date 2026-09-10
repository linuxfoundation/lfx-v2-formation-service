// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package mock provides hand-written port doubles, one per port, selected by
// REPOSITORY_SOURCE=mock in cmd/formation-api/service/providers.go. No
// mock-generation tooling is used, matching lfx-v2-committee-service's
// internal/infrastructure/mock/ convention.
package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// FormationRepository is an in-memory port.FormationRepository double, keyed
// on project UID the same way the real uniqueness constraint is.
type FormationRepository struct {
	Recorder
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
	r.record("formations.Create")
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
	if clone.Sections == nil {
		clone.Sections = []model.FormationSection{}
	}
	clone.Revision = 1
	r.byUID[clone.UID] = &clone
	r.byProject[clone.ProjectUID] = clone.UID

	out := clone
	return &out, nil
}

// GetByProject returns the formation for a project, or domain.ErrNotFound.
func (r *FormationRepository) GetByProject(_ context.Context, projectUID string) (*model.Formation, error) {
	r.record("formations.GetByProject")
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
	r.record("formations.UpdateLifecycle")
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
	// Mirrors the repository: completing stamps the time, returning to live
	// clears it, and freezing leaves whatever is there — a checklist that
	// completed before being archived did complete.
	switch lifecycle {
	case model.LifecycleCompleted:
		now := time.Now().UTC()
		f.CompletedAt = &now
	case model.LifecycleLive:
		f.CompletedAt = nil
	case model.LifecycleFrozen:
	}
	out := *f
	return &out, nil
}

// UpdateSections replaces the section snapshot, refusing the write when
// revision is stale (domain.ErrVersionMismatch), matching the repository.
func (r *FormationRepository) UpdateSections(
	_ context.Context, uid uuid.UUID, sections []model.FormationSection, revision int64,
) (*model.Formation, error) {
	r.record("formations.UpdateSections")
	r.mu.Lock()
	defer r.mu.Unlock()

	f, ok := r.byUID[uid]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if f.Revision != revision {
		return nil, domain.ErrVersionMismatch
	}

	f.Sections = sections
	f.Revision++
	out := *f
	return &out, nil
}

// ListProjectUIDs returns every project that already has a formation.
func (r *FormationRepository) ListProjectUIDs(_ context.Context) ([]string, error) {
	r.record("formations.ListProjectUIDs")
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.byProject))
	for projectUID := range r.byProject {
		out = append(out, projectUID)
	}
	return out, nil
}

// MarkNotified sets one notification timestamp where it is still nil, matching
// the at-most-once semantics of the Postgres implementation. It returns
// acquired=true when this call set the timestamp (equivalent to RowsAffected>0
// in the real DB) and acquired=false when the column was already set.
func (r *FormationRepository) MarkNotified(_ context.Context, uid uuid.UUID, column string) (bool, error) {
	r.record("formations.MarkNotified")
	r.mu.Lock()
	defer r.mu.Unlock()

	f, ok := r.byUID[uid]
	if !ok {
		return false, domain.ErrNotFound
	}

	now := time.Now().UTC()
	switch column {
	case "notified_activating_at":
		if f.NotifiedActivatingAt != nil {
			return false, nil
		}
		f.NotifiedActivatingAt = &now
	case "notified_reminder_3d_at":
		if f.NotifiedReminderThreeDayAt != nil {
			return false, nil
		}
		f.NotifiedReminderThreeDayAt = &now
	case "notified_reminder_overdue_at":
		if f.NotifiedReminderOverdueAt != nil {
			return false, nil
		}
		f.NotifiedReminderOverdueAt = &now
	default:
		return false, fmt.Errorf("MarkNotified: unknown column %q", column)
	}
	return true, nil
}
