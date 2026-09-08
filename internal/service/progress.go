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
// own bucket here and is never folded into done: a skipped item did not get
// done, it got excused.
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

// countsFromItems tallies statuses from the items already loaded for the
// response, rather than asking the database for the same numbers again.
// Besides saving a round trip, it is what makes the tally consistent with
// the items beside it: the item list and the aggregate used to be separate
// statements with no shared snapshot, so a mutation landing between them
// could return an item in one state and a progress count from another.
func countsFromItems(items []*model.Item) map[model.ItemStatus]int {
	counts := make(map[model.ItemStatus]int, 6)
	for _, item := range items {
		counts[item.Status]++
	}
	return counts
}

// gateSummaryFromItems reports how many gating items exist and how many are
// not yet done, from that same loaded slice: gate items only, outstanding
// when status is anything other than done — a skipped gating item was
// excused, not completed, so it still counts as outstanding.
func gateSummaryFromItems(items []*model.Item) (total, outstanding int) {
	for _, item := range items {
		if !item.Gate {
			continue
		}
		total++
		if item.Status != model.StatusDone {
			outstanding++
		}
	}
	return total, outstanding
}
