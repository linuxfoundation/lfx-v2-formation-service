// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// ApplicationRepository is an in-memory port.ApplicationRepository double.
type ApplicationRepository struct {
	Recorder
	mu   sync.Mutex
	apps map[uuid.UUID]*model.Application
}

// NewApplicationRepository constructs an empty double.
func NewApplicationRepository() *ApplicationRepository {
	return &ApplicationRepository{
		apps: map[uuid.UUID]*model.Application{},
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
	_ context.Context, uid uuid.UUID, payload map[string]any,
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
	stored.Payload = payload
	stored.UpdatedAt = time.Now().UTC()

	a.Payload = payload
	a.UpdatedAt = stored.UpdatedAt
	return a, nil
}

// Transition moves the state.
func (r *ApplicationRepository) Transition(
	_ context.Context, uid uuid.UUID, to model.ApplicationState,
) (*model.Application, error) {
	r.record("applications.Transition")
	r.mu.Lock()
	defer r.mu.Unlock()

	a, err := r.lookup(uid)
	if err != nil {
		return nil, err
	}
	stored := r.apps[uid]
	stored.State = to
	stored.UpdatedAt = time.Now().UTC()

	a.State = to
	a.UpdatedAt = stored.UpdatedAt
	return a, nil
}

// Delete removes the application.
func (r *ApplicationRepository) Delete(_ context.Context, uid uuid.UUID) error {
	r.record("applications.Delete")
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.apps[uid]; !ok {
		return domain.ErrNotFound
	}
	delete(r.apps, uid)
	return nil
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
