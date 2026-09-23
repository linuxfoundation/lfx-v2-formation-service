// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// Postgres-backed, so these skip without FORMATION_TEST_DATABASE_URL — see
// testPool in schema_test.go.

func newApplication() *model.Application {
	return &model.Application{
		SubmitterUsername: "asmith",
		SubmitterName:     "A Smith",
		SubmitterEmail:    "asmith@example.test",
		Payload: map[string]any{
			"project_name":    "Proposed Project",
			"project_website": "https://proposed.example.test",
			"formation_list":  []any{"one@example.test", "two@example.test"},
		},
	}
}

func createApplication(t *testing.T, repo *ApplicationRepo) *model.Application {
	t.Helper()
	a, err := repo.Create(context.Background(), newApplication())
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	return a
}

func TestApplicationCreateStoresSubmitterAndPayload(t *testing.T) {
	db := testDB(t)
	repo := NewApplicationRepo(db)
	ctx := context.Background()

	created := createApplication(t, repo)
	if created.UID == uuid.Nil {
		t.Fatal("create must assign a uid")
	}

	got, err := repo.Get(ctx, created.UID)
	if err != nil {
		t.Fatalf("get application: %v", err)
	}
	if got.SubmitterEmail != "asmith@example.test" {
		t.Errorf("submitter email = %q, want asmith@example.test", got.SubmitterEmail)
	}
	if got.Payload["project_website"] != "https://proposed.example.test" {
		t.Errorf("payload lost project_website: %#v", got.Payload)
	}
	// The intake asks who else should be on the formation work and takes
	// email addresses. Nothing resolves them to platform identities, so they
	// have to survive as written or the answer is lost.
	if got.Payload["formation_list"] == nil {
		t.Errorf("payload lost formation_list: %#v", got.Payload)
	}
	if got.TargetParentUID != nil {
		t.Errorf("target parent = %v, want nil when none was given", *got.TargetParentUID)
	}
}

func TestApplicationCreateStartsAtRevisionOne(t *testing.T) {
	db := testDB(t)
	created := createApplication(t, NewApplicationRepo(db))

	var revision int64
	err := db.NewRaw(
		"SELECT revision FROM project_applications WHERE uid = ?", created.UID,
	).Scan(context.Background(), &revision)
	if err != nil {
		t.Fatalf("read application revision: %v", err)
	}
	if revision != 1 {
		t.Errorf("revision = %d, want 1", revision)
	}
}

func TestApplicationTransitionMovesTheState(t *testing.T) {
	db := testDB(t)
	repo := NewApplicationRepo(db)
	ctx := context.Background()

	created := createApplication(t, repo)

	decided, err := repo.Transition(
		ctx, created.UID, created.Revision, model.ApplicationAccepted,
	)
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if decided.State != model.ApplicationAccepted {
		t.Errorf("state = %q, want accepted", decided.State)
	}
	if decided.Revision != created.Revision+1 {
		t.Errorf("revision = %d, want %d", decided.Revision, created.Revision+1)
	}
	if _, err := repo.Transition(
		ctx, created.UID, created.Revision, model.ApplicationDenied,
	); !errors.Is(err, domain.ErrVersionMismatch) {
		t.Errorf("transition with old revision = %v, want ErrVersionMismatch", err)
	}

	got, err := repo.Get(ctx, created.UID)
	if err != nil {
		t.Fatalf("get application: %v", err)
	}
	if got.State != model.ApplicationAccepted {
		t.Errorf("stored state = %q, want accepted", got.State)
	}
}

// A transition is only as durable as the transaction carrying it, so this
// aborts after the transition and asserts the state went back.
func TestApplicationTransitionRollsBackWithTheTransaction(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo := NewApplicationRepo(db)
	created := createApplication(t, repo)

	wantErr := errors.New("caller aborted after the transition")
	err := NewUnitOfWork(db).Do(ctx, func(tx port.Tx) error {
		if _, err := tx.Applications().Transition(
			ctx, created.UID, created.Revision, model.ApplicationDenied,
		); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("unit of work error = %v, want %v", err, wantErr)
	}

	got, err := repo.Get(ctx, created.UID)
	if err != nil {
		t.Fatalf("get application: %v", err)
	}
	if got.State != model.ApplicationSubmitted {
		t.Errorf("state = %q, want the pre-transition state after rollback", got.State)
	}
}

func TestApplicationUpdatePayloadLeavesStateAlone(t *testing.T) {
	db := testDB(t)
	repo := NewApplicationRepo(db)
	ctx := context.Background()

	created := createApplication(t, repo)

	revised, err := repo.UpdatePayload(
		ctx, created.UID, created.Revision, map[string]any{"project_name": "Renamed"},
	)
	if err != nil {
		t.Fatalf("update payload: %v", err)
	}
	if revised.Payload["project_name"] != "Renamed" {
		t.Errorf("payload = %#v, want the replacement", revised.Payload)
	}
	if revised.State != created.State {
		t.Errorf("state = %q, want it unchanged at %q", revised.State, created.State)
	}
	if revised.Revision != created.Revision+1 {
		t.Errorf("revision = %d, want %d", revised.Revision, created.Revision+1)
	}
}

// Deleting removes the row.
func TestApplicationDelete(t *testing.T) {
	db := testDB(t)
	repo := NewApplicationRepo(db)
	ctx := context.Background()
	created := createApplication(t, repo)

	marker, err := repo.Delete(ctx, created.UID, created.Revision)
	if err != nil {
		t.Fatalf("delete = %v, want no error", err)
	}
	if marker.UID != created.UID || marker.Revision != created.Revision+1 {
		t.Errorf("deletion marker = %#v, want uid %s revision %d",
			marker, created.UID, created.Revision+1)
	}
	if _, err := repo.Get(ctx, created.UID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}
	deletions, err := repo.ListDeletionPage(ctx, uuid.Nil, 10)
	if err != nil {
		t.Fatalf("list deletion markers: %v", err)
	}
	if len(deletions) != 1 || deletions[0].UID != created.UID {
		t.Errorf("deletion markers = %#v, want marker for %s", deletions, created.UID)
	}
}

func TestApplicationWritesRejectAStaleRevision(t *testing.T) {
	db := testDB(t)
	repo := NewApplicationRepo(db)
	ctx := context.Background()

	t.Run("payload", func(t *testing.T) {
		created := createApplication(t, repo)
		_, err := repo.UpdatePayload(
			ctx, created.UID, created.Revision+1, map[string]any{"project_name": "Stale"},
		)
		if !errors.Is(err, domain.ErrVersionMismatch) {
			t.Errorf("update payload = %v, want ErrVersionMismatch", err)
		}
	})

	t.Run("transition", func(t *testing.T) {
		created := createApplication(t, repo)
		_, err := repo.Transition(
			ctx, created.UID, created.Revision+1, model.ApplicationAccepted,
		)
		if !errors.Is(err, domain.ErrVersionMismatch) {
			t.Errorf("transition = %v, want ErrVersionMismatch", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		created := createApplication(t, repo)
		_, err := repo.Delete(ctx, created.UID, created.Revision+1)
		if !errors.Is(err, domain.ErrVersionMismatch) {
			t.Errorf("delete = %v, want ErrVersionMismatch", err)
		}
	})
}

func TestApplicationGetForUpdateHoldsTheRowUntilCommit(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created := createApplication(t, NewApplicationRepo(db))

	type lockAttempt struct {
		pid int
		err error
	}
	started := make(chan lockAttempt, 1)
	competing := make(chan error, 1)

	err := NewUnitOfWork(db).Do(ctx, func(transaction port.Tx) error {
		if _, err := transaction.Applications().GetForUpdate(ctx, created.UID); err != nil {
			return err
		}

		go func() {
			competing <- NewUnitOfWork(db).Do(ctx, func(transaction port.Tx) error {
				concrete := transaction.(*tx)
				var pid int
				if err := concrete.applications.db.NewRaw("SELECT pg_backend_pid()").Scan(ctx, &pid); err != nil {
					started <- lockAttempt{err: err}
					return err
				}
				started <- lockAttempt{pid: pid}
				_, err := transaction.Applications().GetForUpdate(ctx, created.UID)
				return err
			})
		}()

		attempt := <-started
		if attempt.err != nil {
			return fmt.Errorf("start competing lock: %w", attempt.err)
		}
		return waitForBackendLock(ctx, transaction.(*tx).applications.db, attempt.pid)
	})
	if err != nil {
		t.Fatalf("holding application lock: %v", err)
	}

	select {
	case err := <-competing:
		if err != nil {
			t.Fatalf("competing lock after commit: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("competing lock stayed blocked after commit: %v", ctx.Err())
	}
}

func waitForBackendLock(ctx context.Context, db bun.IDB, pid int) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		var waiting bool
		err := db.NewRaw(`
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid = ? AND wait_event_type = 'Lock'
			)`, pid).Scan(ctx, &waiting)
		if err != nil {
			return fmt.Errorf("inspect competing lock: %w", err)
		}
		if waiting {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for competing lock: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Every write path reports a missing application the same way, so a caller
// never has to tell "gone" apart from "failed".
func TestApplicationAbsentIsNotFound(t *testing.T) {
	db := testDB(t)
	repo := NewApplicationRepo(db)
	ctx := context.Background()
	absent := uuid.New()

	if _, err := repo.Get(ctx, absent); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get = %v, want ErrNotFound", err)
	}
	if _, err := repo.UpdatePayload(ctx, absent, 1, map[string]any{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("update payload = %v, want ErrNotFound", err)
	}
	if _, err := repo.Transition(ctx, absent, 1, model.ApplicationAccepted); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("transition = %v, want ErrNotFound", err)
	}
	if _, err := repo.Delete(ctx, absent, 1); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("delete = %v, want ErrNotFound", err)
	}
}
