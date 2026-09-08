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
	// An empty slice, not nil: the column is NOT NULL DEFAULT '[]', and a nil
	// slice marshals to JSON null rather than [] — the same reason Item's
	// SubItems is defaulted the same way.
	if f.Sections == nil {
		f.Sections = []model.FormationSection{}
	}

	// ON CONFLICT DO NOTHING rather than catching the unique violation, because
	// the violation aborts the surrounding transaction. The caller treats "this
	// project already has a checklist" as success — that is what makes the
	// reconcile safe to run on every replica — and it cannot do that from inside
	// a transaction Postgres has already put beyond saving: the commit then
	// fails with "commit unexpectedly resulted in rollback" and the whole sweep
	// reports an error for a project that is perfectly fine.
	//
	// Letting Postgres absorb the conflict keeps the transaction usable, so the
	// caller's decision to continue actually holds.
	res, err := r.db.NewInsert().
		Model(f).
		On("CONFLICT (project_uid) DO NOTHING").
		Returning("*").
		Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, domain.ErrAlreadyExists
		}
		return nil, fmt.Errorf("insert formation: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("insert formation: %w", err)
	}
	if affected == 0 {
		return nil, domain.ErrAlreadyExists
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
	//
	// Going back to live clears it, because a checklist that has re-entered
	// formation is not complete and a stale timestamp beside a live lifecycle
	// reads as a contradiction. Freezing deliberately leaves it alone: a
	// checklist that completed and was later archived did genuinely complete,
	// and that is history worth keeping rather than a stale value.
	switch lifecycle {
	case model.LifecycleCompleted:
		q = q.Set("completed_at = now()")
	case model.LifecycleLive:
		q = q.Set("completed_at = NULL")
	case model.LifecycleFrozen:
		// Left as it is, per the reasoning above.
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
		// affected==0 means either uid doesn't exist or revision was
		// stale. Disambiguate: a client told 412 "re-read and retry"
		// for an unknown uid would 404 on the re-read and retry forever.
		exists, err := r.db.NewSelect().Model((*model.Formation)(nil)).Where("uid = ?", uid).Exists(ctx)
		if err != nil {
			return nil, fmt.Errorf("probe formation existence: %w", err)
		}
		if !exists {
			return nil, domain.ErrNotFound
		}
		return nil, domain.ErrVersionMismatch
	}
	return f, nil
}

// UpdateSections replaces the section snapshot, refusing the write when
// revision is stale. Mirrors UpdateLifecycle's existence-vs-staleness
// disambiguation for the same reason: a caller acting on ErrVersionMismatch
// needs to know a retry can succeed, which is not true for a uid that is gone.
func (r *FormationRepo) UpdateSections(
	ctx context.Context, uid uuid.UUID, sections []model.FormationSection, revision int64,
) (*model.Formation, error) {
	f := &model.Formation{}
	res, err := r.db.NewUpdate().
		Model(f).
		Set("sections = ?", sections).
		Set("revision = revision + 1").
		Set("updated_at = now()").
		Where("uid = ?", uid).
		Where("revision = ?", revision).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("update formation sections: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		exists, err := r.db.NewSelect().Model((*model.Formation)(nil)).Where("uid = ?", uid).Exists(ctx)
		if err != nil {
			return nil, fmt.Errorf("probe formation existence: %w", err)
		}
		if !exists {
			return nil, domain.ErrNotFound
		}
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
