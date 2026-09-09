// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

func TestSetProjectsByUIDReplacesRatherThanAccumulates(t *testing.T) {
	ctx := context.Background()
	r := NewProjectReader()

	r.SetProjectsByUID([]port.ProjectRef{{UID: "project-1", SubStage: model.StageActive}})
	r.SetProjectsByUID([]port.ProjectRef{{UID: "project-2", SubStage: model.StageActive}})

	got, err := r.ListFormingProjects(ctx, []string{"project-1", "project-2"})
	if err != nil {
		t.Fatalf("ListFormingProjects() = %v, want no error", err)
	}
	if len(got) != 1 || got[0].UID != "project-2" {
		t.Errorf("named both UIDs and got %v, want only project-2 — the second seed replaces the first",
			got)
	}
}

func TestNameCountsItsCalls(t *testing.T) {
	ctx := context.Background()
	r := NewProjectReader()
	r.SetName("project-1", "A Project")

	if _, err := r.Name(ctx, "project-1"); err != nil {
		t.Fatalf("Name() = %v, want no error", err)
	}
	if _, err := r.Name(ctx, "project-1"); err != nil {
		t.Fatalf("second Name() = %v, want no error", err)
	}
	// The count is what a later test asserts on to prove the projection stopped
	// resolving one name per row, so it has to survive a repeated lookup rather
	// than being deduplicated here.
	if got := r.NameCalls(); got != 2 {
		t.Errorf("NameCalls() = %d, want 2", got)
	}

	if _, err := r.Name(ctx, "unseeded"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Name(unseeded) = %v, want domain.ErrNotFound", err)
	}
}
