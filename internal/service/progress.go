// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// progressFromCounts derives the six-way progress count on every read
// rather than maintaining a counter — a status change is always the
// single source of truth, so there is nothing to keep in sync. skipped is its
// own bucket here, matching the item repository's StatusCounts, and is never
// folded into done: a skipped item did not get done, it got excused.
func progressFromCounts(counts map[model.ItemStatus]int) *svc.FormationProgress {
	return &svc.FormationProgress{
		NotStarted:         counts[model.StatusNotStarted],
		InProgress:         counts[model.StatusInProgress],
		Blocked:            counts[model.StatusBlocked],
		AwaitingAcceptance: counts[model.StatusAwaitingAcceptance],
		Done:               counts[model.StatusDone],
		Skipped:            counts[model.StatusSkipped],
	}
}
