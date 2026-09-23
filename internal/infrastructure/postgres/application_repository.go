// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

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

// Delete removes the PII row and leaves a durable cleanup marker atomically.
func (r *ApplicationRepo) Delete(
	ctx context.Context, uid uuid.UUID, revision int64,
) (*model.ApplicationDeletion, error) {
	marker := &model.ApplicationDeletion{
		UID:       uid,
		Revision:  revision + 1,
		DeletedAt: time.Now().UTC(),
	}
	res, err := r.db.NewRaw(`
		WITH deleted AS (
			DELETE FROM project_applications
			WHERE uid = ? AND revision = ?
			RETURNING uid, revision + 1 AS revision
		)
		INSERT INTO project_application_deletions (uid, revision, deleted_at)
		SELECT uid, revision, ? FROM deleted`,
		uid, revision, marker.DeletedAt,
	).Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("delete application: %w", err)
	}
	if err := requireApplicationRevision(ctx, r.db, res, uid, "delete application"); err != nil {
		return nil, err
	}
	return marker, nil
}

func (r *ApplicationRepo) ListRepairPage(
	ctx context.Context, after uuid.UUID, limit int,
) ([]*model.Application, error) {
	applications := make([]*model.Application, 0, limit)
	query := r.db.NewSelect().Model(&applications).OrderExpr("uid ASC").Limit(limit)
	if after != uuid.Nil {
		query = query.Where("uid > ?", after)
	}
	if err := query.Scan(ctx); err != nil {
		return nil, fmt.Errorf("list applications for repair: %w", err)
	}
	return applications, nil
}

func (r *ApplicationRepo) ListDeletionPage(
	ctx context.Context, after uuid.UUID, limit int,
) ([]*model.ApplicationDeletion, error) {
	deletions := make([]*model.ApplicationDeletion, 0, limit)
	query := r.db.NewSelect().Model(&deletions).OrderExpr("uid ASC").Limit(limit)
	if after != uuid.Nil {
		query = query.Where("uid > ?", after)
	}
	if err := query.Scan(ctx); err != nil {
		return nil, fmt.Errorf("list application deletions for repair: %w", err)
	}
	return deletions, nil
}

// UpdatePayload replaces the intake answers, leaving the state alone.
//
// What the answers were before is not recoverable from here. No source asks
// for payload versioning, and applications have no history table.
func (r *ApplicationRepo) UpdatePayload(
	ctx context.Context, uid uuid.UUID, revision int64, payload map[string]any,
) (*model.Application, error) {
	if payload == nil {
		payload = map[string]any{}
	}

	a := &model.Application{}
	res, err := r.db.NewUpdate().
		Model(a).
		Set("payload = ?", payload).
		Set("revision = revision + 1").
		Set("updated_at = now()").
		Where("uid = ?", uid).
		Where("revision = ?", revision).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("update application payload: %w", err)
	}
	if err := requireApplicationRevision(
		ctx, r.db, res, uid, "update application payload",
	); err != nil {
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
	ctx context.Context, uid uuid.UUID, revision int64, to model.ApplicationState,
) (*model.Application, error) {
	a := &model.Application{}
	res, err := r.db.NewUpdate().
		Model(a).
		Set("state = ?", to).
		Set("revision = revision + 1").
		Set("updated_at = now()").
		Where("uid = ?", uid).
		Where("revision = ?", revision).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("update application state: %w", err)
	}
	if err := requireApplicationRevision(
		ctx, r.db, res, uid, "update application state",
	); err != nil {
		return nil, err
	}
	return a, nil
}

func requireApplicationRevision(
	ctx context.Context, db bun.IDB, res sql.Result, uid uuid.UUID, op string,
) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", op, err)
	}
	if affected != 0 {
		return nil
	}

	exists, err := db.NewSelect().
		Model((*model.Application)(nil)).
		Where("uid = ?", uid).
		Exists(ctx)
	if err != nil {
		return fmt.Errorf("%s: check application existence: %w", op, err)
	}
	if !exists {
		return domain.ErrNotFound
	}
	return domain.ErrVersionMismatch
}
