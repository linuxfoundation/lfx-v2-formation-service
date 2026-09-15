// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// statusActionTargets is every status action and the status it moves an item
// to, which is what makes "absent from the list" and "refused by the write
// path" comparable propositions about the same edge.
var statusActionTargets = map[string]model.ItemStatus{
	model.ActionMarkInProgress:   model.StatusInProgress,
	model.ActionMarkDone:         model.StatusDone,
	model.ActionMarkBlocked:      model.StatusBlocked,
	model.ActionSkip:             model.StatusSkipped,
	model.ActionBackToNotStarted: model.StatusNotStarted,
}

// The list is advisory: it describes what the write path would accept, and the
// write path refuses everything the list leaves out whether or not the caller
// ever read it. Asserting the second half is what makes the first half worth
// publishing — a list that over-promised would send consumers into refusals it
// told them would not happen.
//
// Every starting status is reachable from not_started in one move, so each case
// sets up by making that move and then attempts an edge the list omits. Reasons
// travel on the attempt wherever one is required, so that a refusal is evidence
// about the transition rather than about a missing reason.
func TestWritePathRefusesEveryActionAbsentFromTheList(t *testing.T) {
	for _, status := range []model.ItemStatus{
		model.StatusNotStarted,
		model.StatusInProgress,
		model.StatusBlocked,
		model.StatusDone,
		model.StatusSkipped,
	} {
		offered := map[string]bool{}
		for _, action := range model.AvailableActionsFor(status, model.LifecycleLive) {
			offered[action.Action] = true
		}

		for action, target := range statusActionTargets {
			if offered[action] {
				continue
			}

			t.Run(string(status)+"/"+action, func(t *testing.T) {
				s, formation, item, _ := newItemMutatorTestService(t)
				ctx := asPrincipal("staffer")

				// Reach the starting status, unless it is where items begin.
				if status != model.StatusNotStarted {
					moved, err := setStatus(s, ctx, &svc.SetItemStatusPayload{
						ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey,
						IfMatch: item.Revision,
						Status:  ptr(string(status)), Reason: ptr("setting up"),
					})
					require.NoError(t, err, "could not reach %s", status)
					require.Equal(t, string(status), moved.Status)
				}

				current, err := s.items.GetByKey(context.Background(), formation.UID, item.ItemKey)
				require.NoError(t, err)

				_, err = setStatus(s, ctx, &svc.SetItemStatusPayload{
					ProjectUID: formation.ProjectUID, ItemKey: item.ItemKey,
					IfMatch: current.Revision,
					Status:  ptr(string(target)), Reason: ptr("attempting an edge the list omits"),
				})

				require.Error(t, err, "%s from %s is absent from the list but the write path allowed it",
					action, status)
			})
		}
	}
}

// Deliberately a verification rather than a change: a diff here is a failure
// signal. The rules function must be reachable from the read path and nowhere
// else, because a write consulting it would turn an advisory description into
// an enforcement path — and then the two could never disagree, which is exactly
// the disagreement the other tests exist to detect.
func TestOnlyTheReadPathConsultsTheRulesFunction(t *testing.T) {
	const rulesFunction = "AvailableActionsFor"

	// The read path, and the only file in this package allowed to call it.
	allowed := map[string]bool{
		"checklist_reader.go": true,
	}

	var callers []string
	err := filepath.WalkDir("..", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The function's own package defines and tests it; the question is who
		// calls it from outside.
		if filepath.Base(filepath.Dir(path)) == "model" {
			return nil
		}

		content, readErr := os.ReadFile(path) //nolint:gosec // walking this repository's own source
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(content), rulesFunction) && !allowed[filepath.Base(path)] {
			callers = append(callers, path)
		}
		return nil
	})
	require.NoError(t, err)

	assert.Empty(t, callers,
		"%s must be reachable from the read path only; these also reference it: %v",
		rulesFunction, callers)
}
