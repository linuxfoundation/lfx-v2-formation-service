// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"testing"

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
