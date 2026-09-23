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

func TestApplicationTransitionMovesTheState(t *testing.T) {
	db := testDB(t)
	repo := NewApplicationRepo(db)
	ctx := context.Background()

	created := createApplication(t, repo)

	decided, err := repo.Transition(ctx, created.UID, model.ApplicationAccepted)
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if decided.State != model.ApplicationAccepted {
		t.Errorf("state = %q, want accepted", decided.State)
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
			ctx, created.UID, model.ApplicationDenied,
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

	revised, err := repo.UpdatePayload(ctx, created.UID, map[string]any{"project_name": "Renamed"})
	if err != nil {
		t.Fatalf("update payload: %v", err)
	}
	if revised.Payload["project_name"] != "Renamed" {
		t.Errorf("payload = %#v, want the replacement", revised.Payload)
	}
	if revised.State != created.State {
		t.Errorf("state = %q, want it unchanged at %q", revised.State, created.State)
	}
}

// Deleting removes the row.
func TestApplicationDelete(t *testing.T) {
	db := testDB(t)
	repo := NewApplicationRepo(db)
	ctx := context.Background()
	created := createApplication(t, repo)

	if err := repo.Delete(ctx, created.UID); err != nil {
		t.Fatalf("delete = %v, want no error", err)
	}
	if _, err := repo.Get(ctx, created.UID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}
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
	if _, err := repo.UpdatePayload(ctx, absent, map[string]any{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("update payload = %v, want ErrNotFound", err)
	}
	if _, err := repo.Transition(ctx, absent, model.ApplicationAccepted); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("transition = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, absent); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("delete = %v, want ErrNotFound", err)
	}
}
