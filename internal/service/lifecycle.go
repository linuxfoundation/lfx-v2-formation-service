// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// Lifecycler moves a checklist's lifecycle to match its project's stage.
//
// A checklist is never deleted, whatever happens to the project. Leaving
// formation moves the lifecycle and nothing else, so the items and their history
// stay exactly as they were — which is what makes re-entering formation restore
// the same checklist rather than build a new one. There is no "restore" path
// here for that reason: coming back is the same transition as any other, from
// frozen or completed back to live.
type Lifecycler struct {
	formations port.FormationRepository
}

// NewLifecycler wires a lifecycler.
func NewLifecycler(formations port.FormationRepository) *Lifecycler {
	return &Lifecycler{formations: formations}
}

// SyncTo brings projectUID's checklist into the lifecycle its stage implies and
// reports whether anything moved.
//
// A project with no checklist is not an error: most projects never have one, and
// the reconcile calls this for every project it sweeps.
func (l *Lifecycler) SyncTo(ctx context.Context, projectUID, stage string) (bool, error) {
	want, ok := model.LifecycleForStage(stage)
	if !ok {
		// An unreadable stage must not be treated as "no longer forming".
		// Freezing a live checklist because a value could not be parsed would
		// lock people out of work in progress, so nothing happens and the next
		// reconcile tries again.
		slog.WarnContext(ctx, "unrecognised project stage; lifecycle left as it is",
			"project_uid", projectUID, "stage", stage)
		return false, nil
	}

	formation, err := l.formations.GetByProject(ctx, projectUID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("reading formation for %s: %w", projectUID, err)
	}

	if formation.Lifecycle == want {
		return false, nil
	}

	from := formation.Lifecycle
	if _, err := l.formations.UpdateLifecycle(ctx, formation.UID, want, formation.Revision); err != nil {
		// A revision mismatch means something else moved this formation between
		// the read and the write. On a loop that runs on every replica every
		// fifteen minutes that is expected rather than exceptional, and the
		// next pass reads the newer revision and agrees with it.
		if errors.Is(err, domain.ErrVersionMismatch) {
			slog.InfoContext(ctx, "lifecycle changed underneath us; leaving it to the next pass",
				"project_uid", projectUID, "stage", stage)
			return false, nil
		}
		return false, fmt.Errorf("moving %s to %s: %w", projectUID, want, err)
	}

	slog.InfoContext(ctx, "formation lifecycle moved",
		"project_uid", projectUID, "stage", stage, "from", from, "to", want)
	return true, nil
}
