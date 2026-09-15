// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	formationserver "github.com/linuxfoundation/lfx-v2-formation-service/gen/http/lfx_v2_formation_service/server"
	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// The read path must carry the list, and the list an item offers has to be the
// one the domain computes for its status. A reader that quietly dropped the
// field, or computed it from something other than the item, would leave every
// consumer inferring controls again — which is the thing this feature removes.
func TestGetFormationCarriesAvailableActions(t *testing.T) {
	s, formation := newChecklistTestService(t)

	result, err := s.GetFormation(context.Background(), &svc.GetFormationPayload{ProjectUID: formation.ProjectUID})

	require.NoError(t, err)
	require.Len(t, result.Items, 1)

	want := model.AvailableActionsFor(model.StatusNotStarted, model.LifecycleLive)
	got := result.Items[0].AvailableActions

	require.Len(t, got, len(want))
	for i, action := range want {
		assert.Equal(t, action.Action, got[i].Action)
		assert.Equal(t, action.RequiresReason, got[i].RequiresReason)
		assert.Equal(t, action.RequiresRelation, got[i].RequiresRelation)
	}
}

// An empty list has to reach the wire as [] and not null, because the attribute
// is required and a consumer must be able to tell "nothing is permitted here"
// from "this build does not send the field". A nil Go slice marshals to null,
// so the distinction rests entirely on the reader initialising the slice empty
// — assert the marshalled bytes rather than the Go value, since only the bytes
// show the difference.
func TestEmptyAvailableActionsReachTheWireAsArray(t *testing.T) {
	s, formation := newChecklistTestService(t)

	// A completed checklist refuses every write, so every item's list is empty
	// while the items themselves are still read and returned.
	_, err := s.formations.UpdateLifecycle(
		context.Background(), formation.UID, model.LifecycleCompleted, formation.Revision)
	require.NoError(t, err)

	result, err := s.GetFormation(context.Background(), &svc.GetFormationPayload{ProjectUID: formation.ProjectUID})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	require.Empty(t, result.Items[0].AvailableActions)

	encoded, err := json.Marshal(result.Items[0].AvailableActions)
	require.NoError(t, err)
	assert.Equal(t, "[]", string(encoded), "an empty list must marshal as [] and not null")

	// And through Goa's generated HTTP response shape, which is how it actually
	// travels: the snake_case attribute must be present, not omitted.
	viewed := svc.NewViewedFormationChecklist(result, "default")
	response := formationserver.NewGetFormationResponseBody(viewed.Projected)
	require.Len(t, response.Items, 1)
	itemJSON, err := json.Marshal(response.Items[0])
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(itemJSON), `"available_actions":[]`),
		"available_actions must be present and empty, got %s", itemJSON)
}
