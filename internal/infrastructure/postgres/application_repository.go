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

// ApplicationRepo persists project applications. It takes a bun.IDB rather
// than a *bun.DB so the same code serves both the pool and a transaction.
type ApplicationRepo struct {
	db bun.IDB
}

// NewApplicationRepo wires an application repository over the given handle.
func NewApplicationRepo(db bun.IDB) *ApplicationRepo {
	return &ApplicationRepo{db: db}
}

// Create inserts an application.
func (r *ApplicationRepo) Create(
	ctx context.Context, a *model.Application,
) (*model.Application, error) {
	if a.State == "" {
		a.State = model.ApplicationSubmitted
	}
	// An empty map, not nil: the column is NOT NULL and a nil map marshals to
	// JSON null rather than {}, which then fails the constraint on a payload
	// the caller simply left empty.
	if a.Payload == nil {
		a.Payload = map[string]any{}
	}

	if _, err := r.db.NewInsert().Model(a).Returning("*").Exec(ctx); err != nil {
		return nil, fmt.Errorf("insert application: %w", err)
	}

	return a, nil
}

// Get returns one application.
func (r *ApplicationRepo) Get(ctx context.Context, uid uuid.UUID) (*model.Application, error) {
	return r.get(ctx, uid, false)
}

// GetForUpdate returns one application, holding the row until the transaction
// ends.
//
// The lock serializes concurrent database writes for this application.
func (r *ApplicationRepo) GetForUpdate(ctx context.Context, uid uuid.UUID) (*model.Application, error) {
	return r.get(ctx, uid, true)
}

func (r *ApplicationRepo) get(ctx context.Context, uid uuid.UUID, lock bool) (*model.Application, error) {
	a := &model.Application{}
	q := r.db.NewSelect().Model(a).Where("uid = ?", uid)
	if lock {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("select application: %w", err)
	}
	return a, nil
}

// Delete removes the application.
//
// A hard delete. No source expresses a preference between hard and soft, and
// a soft delete would need a rule for what reads see and how long rows are
// kept, neither of which is specified — so the simpler behaviour is the one
// that makes no unstated promise.
func (r *ApplicationRepo) Delete(ctx context.Context, uid uuid.UUID) error {
	res, err := r.db.NewDelete().
		Model((*model.Application)(nil)).
		Where("uid = ?", uid).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete application: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete application rows affected: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// UpdatePayload replaces the intake answers, leaving the state alone.
//
// What the answers were before is not recoverable from here. No source asks
// for payload versioning, and applications have no history table.
func (r *ApplicationRepo) UpdatePayload(
	ctx context.Context, uid uuid.UUID, payload map[string]any,
) (*model.Application, error) {
	if payload == nil {
		payload = map[string]any{}
	}

	a := &model.Application{}
	res, err := r.db.NewUpdate().
		Model(a).
		Set("payload = ?", payload).
		Set("updated_at = now()").
		Where("uid = ?", uid).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("update application payload: %w", err)
	}
	if err := requireOneRow(res, "update application payload"); err != nil {
		return nil, err
	}
	return a, nil
}

// Transition moves the state.
//
// The UPDATE carries no expected-current-state predicate, so this will move an
// application from any state to any other. That is deliberate: no agreed
// design enumerates the legal transitions, and a predicate here would turn
// this service's guess into a refusal the caller cannot explain. Whatever
// ordering rules do get agreed belong in the use case, where the refusal can
// name itself.
func (r *ApplicationRepo) Transition(
	ctx context.Context, uid uuid.UUID, to model.ApplicationState,
) (*model.Application, error) {
	a := &model.Application{}
	res, err := r.db.NewUpdate().
		Model(a).
		Set("state = ?", to).
		Set("updated_at = now()").
		Where("uid = ?", uid).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("update application state: %w", err)
	}
	if err := requireOneRow(res, "update application state"); err != nil {
		return nil, err
	}
	return a, nil
}

// requireOneRow maps an update or delete that matched nothing onto
// ErrNotFound. Applications have no optimistic lock, so unlike the formation
// writes there is no second explanation for zero rows to disambiguate.
func requireOneRow(res sql.Result, op string) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", op, err)
	}
	if affected == 0 {
		return domain.ErrNotFound
	}
	return nil
}
