// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// uniqueViolation is the Postgres SQLSTATE for a unique constraint breach.
const uniqueViolation = "23505"

// FormationRepo persists formations. It takes a bun.IDB rather than a *bun.DB
// so the same code serves both the pool and a transaction.
type FormationRepo struct {
	db bun.IDB
}

// NewFormationRepo wires a formation repository over the given handle.
func NewFormationRepo(db bun.IDB) *FormationRepo {
	return &FormationRepo{db: db}
}

// Create inserts a formation, returning domain.ErrAlreadyExists when one
// already exists for the project.
//
// The uniqueness constraint is the concurrency control, not a preceding read:
// with the reconcile loop running on every replica, a read-then-write check
// would let two replicas both observe "absent" and both insert. Letting the
// database refuse the second is what makes the loop safe to run everywhere.
func (r *FormationRepo) Create(ctx context.Context, f *model.Formation) (*model.Formation, error) {
	if f.Lifecycle == "" {
		f.Lifecycle = model.LifecycleLive
	}
	if f.Revision == 0 {
		f.Revision = 1
	}

	if _, err := r.db.NewInsert().
		Model(f).
		Returning("*").
		Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return nil, domain.ErrAlreadyExists
		}
		return nil, fmt.Errorf("insert formation: %w", err)
	}
	return f, nil
}

// GetByProject returns the formation for a project.
func (r *FormationRepo) GetByProject(ctx context.Context, projectUID string) (*model.Formation, error) {
	f := &model.Formation{}
	err := r.db.NewSelect().
		Model(f).
		Where("project_uid = ?", projectUID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("select formation: %w", err)
	}
	return f, nil
}

// UpdateLifecycle moves the formation's lifecycle under an optimistic lock.
// Zero rows affected means the caller's revision was stale.
func (r *FormationRepo) UpdateLifecycle(ctx context.Context, uid uuid.UUID, lifecycle model.Lifecycle, revision int64) (*model.Formation, error) {
	f := &model.Formation{}
	q := r.db.NewUpdate().
		Model(f).
		Set("lifecycle = ?", lifecycle).
		Set("revision = revision + 1").
		Set("updated_at = now()").
		Where("uid = ?", uid).
		Where("revision = ?", revision).
		Returning("*")

	// completed_at is part of becoming completed, so it moves in the same
	// statement rather than in a second write that could fail on its own.
	if lifecycle == model.LifecycleCompleted {
		q = q.Set("completed_at = now()")
	}

	res, err := q.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("update formation lifecycle: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return nil, domain.ErrVersionMismatch
	}
	return f, nil
}

// ListProjectUIDs returns the projects that already have a formation, so the
// reconcile loop can diff against the forming projects and create the rest.
func (r *FormationRepo) ListProjectUIDs(ctx context.Context) ([]string, error) {
	var uids []string
	err := r.db.NewSelect().
		Model((*model.Formation)(nil)).
		Column("project_uid").
		Scan(ctx, &uids)
	if err != nil {
		return nil, fmt.Errorf("list formation project uids: %w", err)
	}
	return uids, nil
}

// isUniqueViolation reports whether err is a Postgres unique constraint
// breach, which callers treat as "someone else got there first".
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == uniqueViolation
	}
	return false
}
