// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"fmt"

	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"
	indexerTypes "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/types"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// IndexerPublisher publishes formation projections for the indexer to store.
//
// Fire-and-forget over core NATS, matching every other publisher in the
// workspace: the indexer answers nothing, and waiting for an acknowledgement it
// does not send would only add a timeout to a path that already tolerates
// failure. The reconcile republishes on every sweep, so a message lost here is
// re-sent within a tick rather than needing recovery.
type IndexerPublisher struct {
	client *Client
}

// Compile-time check that this satisfies the port.
var _ port.IndexerPublisher = (*IndexerPublisher)(nil)

// NewIndexerPublisher wires a publisher over the shared NATS client.
func NewIndexerPublisher(client *Client) *IndexerPublisher {
	return &IndexerPublisher{client: client}
}

// PublishFormation upserts one checklist's projection.
//
// Always the "updated" action, never "created". The indexer upserts on either,
// and the distinction only decides whether it stamps created_by or updated_by —
// so claiming creation on a republish would rewrite the creation record of a
// document that already existed. The sweep cannot tell the two apart, and the
// republish is by far the more common case.
func (p *IndexerPublisher) PublishFormation(ctx context.Context, doc *port.FormationProjection) error {
	if doc == nil {
		return fmt.Errorf("nil projection")
	}
	if doc.ProjectUID == "" {
		// Refused rather than published: the access check is built from this
		// UID, and a document declaring access against "project:" would be
		// readable by nobody or, worse, match something unintended.
		return fmt.Errorf("projection has no project_uid")
	}
	if doc.FormationUID == "" {
		// This becomes indexing_config.object_id, which the indexer requires to
		// be a non-empty string and refuses the message without. Because this
		// publish is fire-and-forget, that refusal arrives as nothing at all —
		// so the row would go missing from the queue with no error anywhere on
		// this side. Caught here, where it is attributable.
		return fmt.Errorf("projection for project %s has no formation_uid", doc.ProjectUID)
	}
	if doc.AccessRelation == "" {
		// Refused rather than defaulted. A default here would be this package
		// quietly deciding who may read a checklist, which is the domain's
		// decision, and the safe-looking default is the dangerous one: the
		// relation every other project document uses is viewer, and viewer is
		// public.
		return fmt.Errorf("projection for project %s declares no access relation", doc.ProjectUID)
	}

	object := projectRefPrefix + doc.ProjectUID
	envelope := indexerMessage{
		Action: indexerConstants.ActionUpdated,
		// The indexer refuses a V2 message with no authorization header, and
		// this publish never has a user behind it. See serviceAccountBearer.
		Headers: serviceAccountHeaders(),
		Data:    projectionData(doc),
		IndexingConfig: &indexerTypes.IndexingConfig{
			ObjectID:            doc.FormationUID,
			AccessCheckObject:   object,
			AccessCheckRelation: doc.AccessRelation,
			HistoryCheckObject:  object,
			// The same relation as the read check. A weaker one would disclose
			// the same data through the audit trail rather than the document.
			HistoryCheckRelation: doc.AccessRelation,
			SortName:             doc.ProjectName,
			NameAndAliases:       nameAndAliases(doc),
			ParentRefs:           parentRefs(doc),
			Tags:                 projectionTags(doc),
		},
	}

	payload, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encoding the formation projection: %w", err)
	}
	return p.client.Publish(ctx, IndexFormationSubject, payload)
}

// DeleteFormation removes one checklist's projection from the index.
//
// The envelope departs from PublishFormation in the one way worth stating
// plainly: Data is the UID itself, as a bare string, where the upsert sends an
// object. The indexer reads the object ID straight out of Data for a delete and
// decodes a body for everything else, so encoding this the way the upsert
// encodes its own would delete nothing. It would also report nothing, because
// the publish is fire-and-forget and a refusal on the far side arrives here as
// silence — which is precisely how an orphaned row survives a repair job that
// looked like it worked.
//
// Sent in the indexer's own envelope, whose Data is typed any and documents
// this case in the field's comment. Restating that shape locally would be this
// service asserting the wire format of a message it does not define.
//
// No indexing_config. Its fields describe who may read the document and how it
// sorts, and there is no document left to read or sort.
func (p *IndexerPublisher) DeleteFormation(ctx context.Context, formationUID string) error {
	if formationUID == "" {
		// The indexer validates the object ID and refuses an empty one, but
		// that refusal is invisible here. Caught where it is attributable.
		return fmt.Errorf("no formation_uid to delete")
	}

	envelope := indexerTypes.IndexerMessageEnvelope{
		Action:  indexerConstants.ActionDeleted,
		Headers: serviceAccountHeaders(),
		Data:    formationUID,
	}

	payload, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encoding the formation deletion: %w", err)
	}
	return p.client.Publish(ctx, IndexFormationSubject, payload)
}

// indexerMessage is the upsert envelope: the indexer's own, narrowed so that
// Data is a map rather than any.
//
// The narrowing is the whole reason this type still exists when the delete
// publishes through indexerTypes.IndexerMessageEnvelope directly. The upsert's
// body is built by projectionData, which returns a map precisely so that adding
// a field to the projection cannot silently publish it, and an any-typed Data
// would accept the port struct itself — publishing every field it grows,
// including ones nobody decided the queue may show. The delete has no body to
// guard, so it has nothing to give up by using the shared type.
//
// Public is deliberately absent. Omitting it leaves the document
// access-controlled, and a formation checklist is never public — see
// formationAccessRelation.
type indexerMessage struct {
	Action         indexerConstants.MessageAction `json:"action"`
	Headers        map[string]string              `json:"headers"`
	Data           map[string]any                 `json:"data"`
	IndexingConfig *indexerTypes.IndexingConfig   `json:"indexing_config,omitempty"`
}

// projectionData is the searchable body.
//
// Written by hand rather than by tagging the port struct, so that adding a field
// to the projection cannot silently publish it: this function is the list of what
// the queue may show, and a reviewer reading it sees exactly that. The keys are
// the wire contract the queue's search filters and sorts on.
func projectionData(doc *port.FormationProjection) map[string]any {
	return map[string]any{
		"formation_uid": doc.FormationUID,
		"project_uid":   doc.ProjectUID,
		"project_name":  doc.ProjectName,
		"project_slug":  doc.ProjectSlug,
		"is_foundation": doc.IsFoundation,
		"parent_uid":    doc.ParentUID,
		"sub_stage":     doc.SubStage,
		"lifecycle":     doc.Lifecycle,
		// Both halves of readiness, under names that say which is which.
		"gates_cleared": doc.GatesCleared,
		"is_activating": doc.IsActivating,
		// Omitted rather than sent empty when unset, so the search's
		// missing-last ordering puts undated rows at the end without the query
		// having to special-case a sentinel.
		"announcement_date": omitEmpty(doc.AnnouncementDate),
		"progress": map[string]any{
			"not_started":         doc.NotStarted,
			"in_progress":         doc.InProgress,
			"blocked":             doc.Blocked,
			"awaiting_acceptance": doc.AwaitingAcceptance,
			"done":                doc.Done,
			"skipped":             doc.Skipped,
		},
		"blocked_item_titles": doc.BlockedItemTitles,
		"assignees":           doc.Assignees,
	}
}

// omitEmpty returns nil for an empty string so the field is absent from the
// document rather than present and empty.
func omitEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nameAndAliases feeds the indexer's name search. The slug is included because
// staff paste slugs, and it is the identifier that appears in URLs.
func nameAndAliases(doc *port.FormationProjection) []string {
	out := make([]string, 0, 2)
	if doc.ProjectName != "" {
		out = append(out, doc.ProjectName)
	}
	if doc.ProjectSlug != "" {
		out = append(out, doc.ProjectSlug)
	}
	return out
}

// parentRefs names the parent project when there is one, which is how the
// indexer models hierarchy.
func parentRefs(doc *port.FormationProjection) []string {
	if doc.ParentUID == "" {
		return nil
	}
	return []string{projectRefPrefix + doc.ParentUID}
}

// projectionTags are the exact-match filters the queue uses.
//
// Assignees are tagged because "Mine" is a filter rather than a full-text
// search, and a tag is what the generic search endpoint can match exactly. They
// are already in the document body, so tagging discloses nothing further.
func projectionTags(doc *port.FormationProjection) []string {
	tags := make([]string, 0, 4+len(doc.Assignees))
	tags = append(tags,
		"project_uid:"+doc.ProjectUID,
		"lifecycle:"+doc.Lifecycle,
	)
	if doc.ProjectSlug != "" {
		tags = append(tags, "project_slug:"+doc.ProjectSlug)
	}
	if doc.SubStage != "" {
		tags = append(tags, "sub_stage:"+doc.SubStage)
	}
	for _, a := range doc.Assignees {
		tags = append(tags, "assignee:"+a)
	}
	return tags
}
