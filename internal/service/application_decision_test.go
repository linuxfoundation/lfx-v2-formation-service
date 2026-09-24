// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// Denying keeps the record, and the record stays readable.
//
// This is the reason denying is the cheap outcome: nothing was created, so
// nothing needs deleting, and deleting a project after the fact is a manual
// process. The row surviving is what makes a later application from the same
// person readable against the earlier decision.
func TestDenyApplicationRetainsTheRecord(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	denied, err := d.service.DenyApplication(asPrincipal("reviewer-one"), &svc.DenyApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})

	require.NoError(t, err)
	assert.Equal(t, string(model.ApplicationDenied), denied.Application.State)

	// Readable afterwards, and carrying the answers it was decided on. A deny
	// that emptied or removed the record would leave nothing to audit.
	stored, err := d.applications.Get(context.Background(), mustUUID(t, created.UID))
	require.NoError(t, err)
	assert.Equal(t, model.ApplicationDenied, stored.State)
	assert.Equal(t, "Proposed Project", stored.Payload["project_name"])

	// Nothing requiring deletion was produced, which is the whole reason
	// denying is the cheap outcome. The grant and the document are still
	// live — the record is meant to stay readable — and the deny published no
	// revocation of either.
	assert.Empty(t, d.access.Deleted(),
		"denying must not revoke access; the retained record has to stay readable")
	assert.Empty(t, d.indexer.ApplicationDeleted(),
		"denying must not remove the document; delete is a separate request")
}

// Accepting records the decision and creates nothing.
func TestAcceptApplicationCreatesNoProject(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	accepted, err := d.service.AcceptApplication(asPrincipal("reviewer-one"), &svc.AcceptApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})

	require.NoError(t, err)
	assert.Equal(t, string(model.ApplicationAccepted), accepted.Application.State)

	// No back-reference is written, because there is no field for one: an
	// accepted application is a decision, not a half-built project, and this
	// service does not track whatever is created from it.
	assert.NotContains(t, accepted.Application.Application, "project_uid")

	stored, err := d.applications.Get(context.Background(), mustUUID(t, created.UID))
	require.NoError(t, err)
	assert.Equal(t, model.ApplicationAccepted, stored.State)
}

// Both decisions land on the record, and the record is all that changes.
func TestEachDecisionRecordsItsState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		act   func(*Service, string) error
		state model.ApplicationState
	}{
		{"accept", func(s *Service, uid string) error {
			_, err := s.AcceptApplication(asPrincipal("reviewer-one"),
				&svc.AcceptApplicationPayload{Version: "1", UID: uid, IfMatch: 1})
			return err
		}, model.ApplicationAccepted},
		{"deny", func(s *Service, uid string) error {
			_, err := s.DenyApplication(asPrincipal("reviewer-one"),
				&svc.DenyApplicationPayload{Version: "1", UID: uid, IfMatch: 1})
			return err
		}, model.ApplicationDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := applicationService(t)
			created := submitOne(t, d.service)

			require.NoError(t, tc.act(d.service, created.UID))

			stored, err := d.applications.Get(context.Background(), mustUUID(t, created.UID))
			require.NoError(t, err)
			assert.Equal(t, tc.state, stored.State)
		})
	}
}

func TestDecisionCanReplaceAnEarlierDecision(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	_, err := d.service.DenyApplication(asPrincipal("reviewer-one"), &svc.DenyApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})
	require.NoError(t, err)

	accepted, err := d.service.AcceptApplication(asPrincipal("reviewer-two"), &svc.AcceptApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision + 1,
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.ApplicationAccepted), accepted.Application.State)
}

// A decision republishes the document, because the state a reviewer filters
// the queue on is the one the document carries.
func TestADecisionRepublishesTheDocument(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	_, err := d.service.DenyApplication(asPrincipal("reviewer-one"), &svc.DenyApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})
	require.NoError(t, err)

	latest := d.indexer.LatestApplication(created.UID)
	require.NotNil(t, latest, "the decision must reach the index; a queue showing the old state "+
		"sends a reviewer back to an application already decided")
	assert.Equal(t, string(model.ApplicationDenied), latest.State)

	stored, err := d.applications.Get(context.Background(), mustUUID(t, created.UID))
	require.NoError(t, err)
	assert.Equal(t, model.ApplicationDenied, stored.State)
}

func TestAcceptApplicationRejectsAStaleRevision(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	_, err := d.service.AcceptApplication(asPrincipal("reviewer-one"), &svc.AcceptApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision + 1,
	})

	var refusal *svc.ApplicationError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "412", refusal.Code)
	assert.Equal(t, "version_mismatch", refusal.Reason)
}
