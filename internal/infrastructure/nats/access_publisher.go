// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"fmt"

	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// The operation names fga-sync dispatches on. It refuses a message whose
// operation does not match the handler the subject routed it to, so these and
// the subjects they travel on have to agree.
const (
	operationUpdateAccess = "update_access"
	operationDeleteAccess = "delete_access"
)

// teamMemberRef names a team's members the way the model's [team#member]
// type restriction requires. fga-sync passes a reference value containing a
// colon through unchanged, so the userset arrives intact.
func teamMemberRef(team string) string {
	return teamRefPrefix + team + "#member"
}

// AccessPublisher writes application grants through lfx-v2-fga-sync, which
// owns every write to the tuple store.
//
// Fire-and-forget over core NATS, like every other publisher here. The
// application repair lane republishes lost live grants and retries retained
// deletion markers.
type AccessPublisher struct {
	client *Client
}

// Compile-time check that this satisfies the port.
var _ port.AccessPublisher = (*AccessPublisher)(nil)

// NewAccessPublisher wires a publisher over the shared NATS client.
func NewAccessPublisher(client *Client) *AccessPublisher {
	return &AccessPublisher{client: client}
}

// PublishApplicationAccess sets the complete grant set on one application.
//
// Full sync, not a patch: fga-sync removes any relation the message does not
// carry and that is not named in exclude_relations. Both relations therefore
// travel on every call, including calls that only meant to change one.
func (p *AccessPublisher) PublishApplicationAccess(ctx context.Context, access port.ApplicationAccess) error {
	if access.ApplicationUID == "" {
		return fmt.Errorf("application access has no uid")
	}
	if access.SubmitterUsername == "" {
		// Refused rather than published without it. The submitter's grant is
		// the only reason the applicant can reach their own application, and
		// a full sync that omitted it would not merely fail to add it — it
		// would delete the one already there.
		return fmt.Errorf("application access for %s names no submitter", access.ApplicationUID)
	}
	if access.FormationTeam == "" {
		// Refused for the mirror-image reason: without it no staff member can
		// review the application, and a sync missing it revokes the team's
		// standing on every application it touches.
		return fmt.Errorf("application access for %s names no formation team", access.ApplicationUID)
	}

	message := fgatypes.GenericFGAMessage{
		ObjectType: applicationObjectType,
		Operation:  operationUpdateAccess,
		Data: fgatypes.GenericAccessData{
			UID: access.ApplicationUID,
			// Never public. An unannounced proposal carries a legal contact's
			// name and email, and `public: true` writes `user:*` as viewer.
			Public: false,
			Relations: map[string][]string{
				constants.RelationApplicationSubmitter: {access.SubmitterUsername},
			},
			// A reference rather than a relation, because the grantee is a
			// userset and not a username: Relations values are prefixed with
			// `user:` on the far side, References values containing a colon
			// are passed through as they are.
			References: map[string][]string{
				constants.RelationApplicationFormationTeam: {teamMemberRef(access.FormationTeam)},
			},
		},
	}

	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encoding the application access update: %w", err)
	}
	return p.client.Publish(ctx, FGAUpdateAccessSubject, payload)
}

// DeleteApplicationAccess removes the publisher-managed grants on one
// application.
//
// The submitter's relation goes; the formation team's does not. fga-sync
// preserves every tuple whose subject is a team userset, including this
// application's formation_team tuple.
func (p *AccessPublisher) DeleteApplicationAccess(ctx context.Context, applicationUID string) error {
	if applicationUID == "" {
		return fmt.Errorf("no application uid to revoke access for")
	}

	message := fgatypes.GenericFGAMessage{
		ObjectType: applicationObjectType,
		Operation:  operationDeleteAccess,
		Data:       fgatypes.GenericDeleteData{UID: applicationUID},
	}

	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encoding the application access removal: %w", err)
	}
	return p.client.Publish(ctx, FGADeleteAccessSubject, payload)
}
