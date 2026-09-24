// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// ApplicationRepository is an in-memory port.ApplicationRepository double.
type ApplicationRepository struct {
	Recorder
	mu        sync.Mutex
	apps      map[uuid.UUID]*model.Application
	deletions map[uuid.UUID]*model.ApplicationDeletion
}

// NewApplicationRepository constructs an empty double.
func NewApplicationRepository() *ApplicationRepository {
	return &ApplicationRepository{
		apps:      map[uuid.UUID]*model.Application{},
		deletions: map[uuid.UUID]*model.ApplicationDeletion{},
	}
}

// Create stores an application.
func (r *ApplicationRepository) Create(
	_ context.Context, a *model.Application,
) (*model.Application, error) {
	r.record("applications.Create")
	r.mu.Lock()
	defer r.mu.Unlock()

	clone := *a
	if clone.UID == uuid.Nil {
		clone.UID = uuid.New()
	}
	if clone.State == "" {
		clone.State = model.ApplicationSubmitted
	}
	if clone.Revision == 0 {
		clone.Revision = 1
	}
	if clone.Payload == nil {
		clone.Payload = map[string]any{}
	}
	now := time.Now().UTC()
	clone.CreatedAt, clone.UpdatedAt = now, now

	r.apps[clone.UID] = &clone

	out := clone
	return &out, nil
}

// Get returns one application.
func (r *ApplicationRepository) Get(_ context.Context, uid uuid.UUID) (*model.Application, error) {
	r.record("applications.Get")
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookup(uid)
}

// GetForUpdate returns one application. There is no locking here: the double
// serialises on its own mutex and mock mode is not where concurrency is
// exercised — that is postgres/application_repository_test.go's job, against a
// real row lock.
func (r *ApplicationRepository) GetForUpdate(ctx context.Context, uid uuid.UUID) (*model.Application, error) {
	r.record("applications.GetForUpdate")
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = ctx
	return r.lookup(uid)
}

// UpdatePayload replaces the intake answers, leaving the state alone.
func (r *ApplicationRepository) UpdatePayload(
	_ context.Context, uid uuid.UUID, revision int64, payload map[string]any,
) (*model.Application, error) {
	r.record("applications.UpdatePayload")
	r.mu.Lock()
	defer r.mu.Unlock()

	a, err := r.lookup(uid)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		payload = map[string]any{}
	}
	stored := r.apps[uid]
	if stored.Revision != revision {
		return nil, domain.ErrVersionMismatch
	}
	stored.Payload = payload
	stored.Revision++
	stored.UpdatedAt = time.Now().UTC()

	a.Payload = payload
	a.Revision = stored.Revision
	a.UpdatedAt = stored.UpdatedAt
	return a, nil
}

// Transition moves the state.
func (r *ApplicationRepository) Transition(
	_ context.Context, uid uuid.UUID, revision int64, to model.ApplicationState,
) (*model.Application, error) {
	r.record("applications.Transition")
	r.mu.Lock()
	defer r.mu.Unlock()

	a, err := r.lookup(uid)
	if err != nil {
		return nil, err
	}
	stored := r.apps[uid]
	if stored.Revision != revision {
		return nil, domain.ErrVersionMismatch
	}
	stored.State = to
	stored.Revision++
	stored.UpdatedAt = time.Now().UTC()

	a.State = to
	a.Revision = stored.Revision
	a.UpdatedAt = stored.UpdatedAt
	return a, nil
}

// Delete removes the application and retains a PII-free deletion marker.
func (r *ApplicationRepository) Delete(
	_ context.Context, uid uuid.UUID, revision int64,
) (*model.ApplicationDeletion, error) {
	r.record("applications.Delete")
	r.mu.Lock()
	defer r.mu.Unlock()

	stored, ok := r.apps[uid]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if stored.Revision != revision {
		return nil, domain.ErrVersionMismatch
	}
	delete(r.apps, uid)
	marker := &model.ApplicationDeletion{
		UID:       uid,
		Revision:  revision + 1,
		DeletedAt: time.Now().UTC(),
	}
	r.deletions[uid] = marker
	copy := *marker
	return &copy, nil
}

// ListRepairPage returns live applications after the UID cursor.
func (r *ApplicationRepository) ListRepairPage(
	_ context.Context, after uuid.UUID, limit int,
) ([]*model.Application, error) {
	r.record("applications.ListRepairPage")
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*model.Application, 0, len(r.apps))
	for uid, application := range r.apps {
		if after != uuid.Nil && uid.String() <= after.String() {
			continue
		}
		copy := *application
		out = append(out, &copy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID.String() < out[j].UID.String() })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListDeletionPage returns retained deletion markers after the UID cursor.
func (r *ApplicationRepository) ListDeletionPage(
	_ context.Context, after uuid.UUID, limit int,
) ([]*model.ApplicationDeletion, error) {
	r.record("applications.ListDeletionPage")
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*model.ApplicationDeletion, 0, len(r.deletions))
	for uid, deletion := range r.deletions {
		if after != uuid.Nil && uid.String() <= after.String() {
			continue
		}
		copy := *deletion
		out = append(out, &copy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID.String() < out[j].UID.String() })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// lookup returns a copy, so a caller mutating the result cannot change stored
// state without going through a write method. Callers hold r.mu.
func (r *ApplicationRepository) lookup(uid uuid.UUID) (*model.Application, error) {
	a, ok := r.apps[uid]
	if !ok {
		return nil, domain.ErrNotFound
	}
	clone := *a
	return &clone, nil
}
