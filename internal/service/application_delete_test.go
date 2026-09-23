// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
)

// Asserted on the stored row and the two publishes that carry the removal
// outward rather than on the response. Both publishes are fire-and-forget and
// a refusal on the far side arrives as silence.
//
// delete_access removes the submitter tuple but deliberately preserves the
// formation-team tuple because fga-sync preserves team-subject grants.
func TestDeleteApplicationRemovesRowDocumentAndSubmitterGrant(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	err := d.service.DeleteApplication(asPrincipal("asmith"), &svc.DeleteApplicationPayload{
		Version: "1", UID: created.UID,
	})
	require.NoError(t, err)

	_, err = d.applications.Get(context.Background(), mustUUID(t, created.UID))
	assert.True(t, errors.Is(err, domain.ErrNotFound), "the row should be gone, got %v", err)

	assert.Equal(t, []string{created.UID}, d.access.Deleted(),
		"the submitter's publisher-managed grant should have been removed")
	assert.Equal(t, []string{created.UID}, d.indexer.ApplicationDeleted(),
		"the indexed document should have been removed")
}

// Both downstream removals are requested. The formation-team tuple is
// intentionally preserved by fga-sync.
func TestDeleteApplicationRequestsBothDownstreamRemovals(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	require.NoError(t, d.service.DeleteApplication(asPrincipal("asmith"),
		&svc.DeleteApplicationPayload{Version: "1", UID: created.UID}))

	require.Len(t, d.access.Deleted(), 1)
	require.Len(t, d.indexer.ApplicationDeleted(), 1)
}

// Deleting is not denying. Denying keeps the record for audit and
// re-application history; deleting removes it. A test for each, so collapsing
// one into the other breaks something.
func TestDenyKeepsWhatDeleteRemoves(t *testing.T) {
	d := applicationService(t)
	denied := submitOne(t, d.service)
	deleted := submitOne(t, d.service)

	_, err := d.service.DenyApplication(asPrincipal("staff"), &svc.DenyApplicationPayload{
		Version: "1", UID: denied.UID,
	})
	require.NoError(t, err)
	require.NoError(t, d.service.DeleteApplication(asPrincipal("staff"),
		&svc.DeleteApplicationPayload{Version: "1", UID: deleted.UID}))

	_, err = d.applications.Get(context.Background(), mustUUID(t, denied.UID))
	assert.NoError(t, err, "a denied application is kept")

	_, err = d.applications.Get(context.Background(), mustUUID(t, deleted.UID))
	assert.True(t, errors.Is(err, domain.ErrNotFound), "a deleted application is gone")
}

// No state gate anywhere. No source enumerates a state set or says which
// actions are legal in which state, so a decided application deletes like any
// other.
func TestDeleteApplicationAcceptsADecidedApplication(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	_, err := d.service.AcceptApplication(asPrincipal("staff"), &svc.AcceptApplicationPayload{
		Version: "1", UID: created.UID,
	})
	require.NoError(t, err)

	assert.NoError(t, d.service.DeleteApplication(asPrincipal("staff"),
		&svc.DeleteApplicationPayload{Version: "1", UID: created.UID}))
}

func TestDeleteApplicationRefusesAnIdentifierThatIsNotAUUID(t *testing.T) {
	d := applicationService(t)

	err := d.service.DeleteApplication(asPrincipal("asmith"), &svc.DeleteApplicationPayload{
		Version: "1", UID: "not-a-uuid",
	})
	require.Error(t, err)
}
