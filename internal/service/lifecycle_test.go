// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// captureLogs points the default logger at a buffer for the duration of a test,
// which is the only way to assert on a symptom that is a log line and nothing
// else.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// The warning exists to surface a stage this service has not been taught, so it
// must not fire for one it has. Prospect is the most common stage on the
// platform and implies no lifecycle at all, so warning about it would bury the
// signal under itself — once per Prospect project, per replica, per sweep.
func TestSyncToIsSilentOnAStageThatSimplyImpliesNothing(t *testing.T) {
	ctx := context.Background()
	logs := captureLogs(t)
	l := NewLifecycler(mock.NewFormationRepository())

	moved, err := l.SyncTo(ctx, "project-1", model.StageProspect)
	if err != nil {
		t.Fatalf("SyncTo() = %v, want no error", err)
	}
	if moved {
		t.Error("moved = true, want false — a Prospect has no checklist to move")
	}
	if strings.Contains(logs.String(), "unrecognised project stage") {
		t.Errorf("Prospect was reported as unrecognised:\n%s", logs.String())
	}
}

// A checklist that re-enters formation is not complete any more, so the
// completion timestamp has to go with the lifecycle. Leaving it behind puts a
// completed_at on a live row, which reads as a contradiction to anything that
// trusts either column.
//
// Freezing is deliberately not the same case: a checklist that completed and was
// then archived did complete, and that is history rather than a stale value.
func TestReturningToFormationClearsTheCompletionButFreezingKeepsIt(t *testing.T) {
	ctx := context.Background()

	for name, tc := range map[string]struct {
		stage        string
		wantCleared  bool
		wantLifecyle model.Lifecycle
	}{
		"back into formation": {model.StageFormationExploratory, true, model.LifecycleLive},
		"archived":            {model.StageArchived, false, model.LifecycleFrozen},
	} {
		t.Run(name, func(t *testing.T) {
			repo := mock.NewFormationRepository()
			formation, err := repo.Create(ctx, &model.Formation{ProjectUID: "project-1"})
			if err != nil {
				t.Fatalf("Create() = %v, want no error", err)
			}

			l := NewLifecycler(repo)
			if _, err := l.SyncTo(ctx, "project-1", model.StageActive); err != nil {
				t.Fatalf("SyncTo(Active) = %v, want no error", err)
			}
			completed, err := repo.GetByProject(ctx, "project-1")
			if err != nil {
				t.Fatalf("GetByProject() = %v, want no error", err)
			}
			if completed.CompletedAt == nil {
				t.Fatal("completed_at after completing = nil, want a timestamp")
			}

			if _, err := l.SyncTo(ctx, "project-1", tc.stage); err != nil {
				t.Fatalf("SyncTo(%s) = %v, want no error", tc.stage, err)
			}
			got, err := repo.GetByProject(ctx, "project-1")
			if err != nil {
				t.Fatalf("GetByProject() = %v, want no error", err)
			}
			if got.Lifecycle != tc.wantLifecyle {
				t.Errorf("lifecycle = %q, want %q", got.Lifecycle, tc.wantLifecyle)
			}
			if cleared := got.CompletedAt == nil; cleared != tc.wantCleared {
				t.Errorf("completed_at cleared = %v, want %v (value %v)",
					cleared, tc.wantCleared, got.CompletedAt)
			}
			_ = formation
		})
	}
}

// The counterpart: a value this service has not been taught must still be
// reported, because it means the upstream enum has moved and nothing here knows.
func TestSyncToStillReportsAStageItCannotRead(t *testing.T) {
	ctx := context.Background()
	logs := captureLogs(t)
	l := NewLifecycler(mock.NewFormationRepository())

	moved, err := l.SyncTo(ctx, "project-1", "Formation-Engaged")
	if err != nil {
		t.Fatalf("SyncTo() = %v, want no error", err)
	}
	if moved {
		t.Error("moved = true, want false")
	}
	if !strings.Contains(logs.String(), "unrecognised project stage") {
		t.Errorf("a near-miss spelling was not reported:\n%s", logs.String())
	}
}
