// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"errors"
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

// accessBlock builds the identity and access/history fields every
// IndexingConfig here carries: the object ID, and the access check mirrored
// onto the history check.
//
// PublishFormation and PublishItem each build one of these from a project
// reference and a relation. Asserting the history-mirrors-access invariant —
// a weaker history guard would disclose the same data through the audit trail
// rather than the document — at each call site let the two drift out of sync
// independently; asserting it once, here, does not.
func accessBlock(objectID, accessObject, relation string) *indexerTypes.IndexingConfig {
	return &indexerTypes.IndexingConfig{
		ObjectID:             objectID,
		AccessCheckObject:    accessObject,
		AccessCheckRelation:  relation,
		HistoryCheckObject:   accessObject,
		HistoryCheckRelation: relation,
	}
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
	config := accessBlock(doc.FormationUID, object, doc.AccessRelation)
	config.SortName = doc.ProjectName
	config.NameAndAliases = nameAndAliases(doc)
	config.ParentRefs = parentRefs(doc)
	config.Tags = projectionTags(doc)

	envelope := indexerMessage{
		Action: indexerConstants.ActionUpdated,
		// The indexer refuses a V2 message with no authorization header, and
		// this publish never has a user behind it. See serviceAccountBearer.
		Headers:        serviceAccountHeaders(),
		Data:           projectionData(doc),
		IndexingConfig: config,
	}

	payload, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encoding the formation projection: %w", err)
	}
	return p.client.Publish(ctx, IndexFormationSubject, payload)
}

// DeleteFormation removes one checklist's projection from the index. See
// deleteDocument for the envelope shape and why it differs from the upsert.
func (p *IndexerPublisher) DeleteFormation(ctx context.Context, formationUID string) error {
	return p.deleteDocument(ctx, IndexFormationSubject, formationUID, "formation_uid")
}

// deleteDocument removes one document from the index, keyed on its own UID as
// a bare string. Shared by DeleteFormation and DeleteItem, which differed only
// in subject and error text.
//
// The envelope departs from an upsert in the one way worth stating plainly:
// Data is the UID itself, as a bare string, where the upsert sends an object.
// The indexer reads the object ID straight out of Data for a delete and
// decodes a body for everything else, so encoding this the way an upsert
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
func (p *IndexerPublisher) deleteDocument(ctx context.Context, subject, uid, what string) error {
	if uid == "" {
		// The indexer validates the object ID and refuses an empty one, but
		// that refusal is invisible here. Caught where it is attributable.
		return fmt.Errorf("no %s to delete", what)
	}

	envelope := indexerTypes.IndexerMessageEnvelope{
		Action:  indexerConstants.ActionDeleted,
		Headers: serviceAccountHeaders(),
		Data:    uid,
	}

	payload, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encoding the %s deletion: %w", what, err)
	}
	return p.client.Publish(ctx, subject, payload)
}

// itemEnvelope validates one item projection and marshals its upsert payload.
// Shared by PublishItem (one flush per call) and PublishItems (one flush per
// batch), so the validation and the wire shape stay identical between the two
// call shapes.
func itemEnvelope(doc *port.ItemProjection) ([]byte, error) {
	if doc == nil {
		return nil, fmt.Errorf("nil item projection")
	}
	if doc.ItemUID == "" {
		// This becomes indexing_config.object_id, which the indexer requires
		// to be a non-empty string and refuses the message without. Caught
		// here, where it is attributable, for the same reason
		// PublishFormation catches an empty FormationUID.
		return nil, fmt.Errorf("item projection has no item_uid")
	}
	if doc.ProjectUID == "" {
		// The access check is built from this UID, and a document declaring
		// access against "project:" would be readable by nobody or, worse,
		// match something unintended.
		return nil, fmt.Errorf("item projection %s has no project_uid", doc.ItemUID)
	}
	if doc.FormationUID == "" {
		return nil, fmt.Errorf("item projection %s has no formation_uid", doc.ItemUID)
	}
	if doc.AccessRelation == "" {
		// Refused rather than defaulted, for the same reason
		// PublishFormation refuses: the relation is the domain's decision,
		// and a document that reused the checklist's own relation via a
		// silent default would stop being reviewable at this layer.
		return nil, fmt.Errorf("item projection %s declares no access relation", doc.ItemUID)
	}

	object := projectRefPrefix + doc.ProjectUID
	config := accessBlock(doc.ItemUID, object, doc.AccessRelation)
	config.SortName = doc.Title
	config.ParentRefs = itemParentRefs(object, doc.FormationUID)
	config.Tags = itemTags(doc)

	envelope := indexerMessage{
		Action:         indexerConstants.ActionUpdated,
		Headers:        serviceAccountHeaders(),
		Data:           newItemProjectionWire(doc),
		IndexingConfig: config,
	}

	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encoding the item projection: %w", err)
	}
	return payload, nil
}

// PublishItem upserts one checklist item's own projection, separate from the
// checklist projection PublishFormation sends for the same sweep.
//
// Always the "updated" action, never "created" — same reasoning as
// PublishFormation: the reconcile sweep cannot tell a genuine first publish
// from a republish of an item that already exists.
func (p *IndexerPublisher) PublishItem(ctx context.Context, doc *port.ItemProjection) error {
	payload, err := itemEnvelope(doc)
	if err != nil {
		return err
	}
	return p.client.Publish(ctx, IndexItemSubject, payload)
}

// PublishItems upserts every item on one checklist in a single round trip:
// N conn.PublishMsg calls followed by one flush, rather than PublishItem's own
// flush per document. Exists for the reconcile sweep's own loop, which already
// holds every item at once; a caller with a single document should still use
// PublishItem.
//
// Best-effort per item, matching the sweep's own contract: one malformed item
// on an otherwise-healthy checklist does not stop its siblings from
// publishing, and a failed item is repaired by the next sweep regardless. Only
// when every item in the batch fails is that surfaced as an error — see
// Projector.Refresh, which uses this to decide whether the checklist's item
// rows published at all, not whether every one of them did.
func (p *IndexerPublisher) PublishItems(ctx context.Context, docs []*port.ItemProjection) error {
	if len(docs) == 0 {
		return nil
	}

	var errs []error
	landed := 0
	for _, doc := range docs {
		payload, err := itemEnvelope(doc)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := p.client.PublishNoFlush(ctx, IndexItemSubject, payload); err != nil {
			errs = append(errs, err)
			continue
		}
		landed++
	}

	if err := p.client.Flush(ctx); err != nil {
		errs = append(errs, err)
	}

	if landed == 0 {
		return fmt.Errorf("no item document published out of %d: %w", len(docs), errors.Join(errs...))
	}
	return nil
}

// DeleteItem removes one item's projection from the index, mirroring
// DeleteFormation exactly. See port.IndexerPublisher.DeleteItem for why this
// has no caller in the sweep today.
func (p *IndexerPublisher) DeleteItem(ctx context.Context, itemUID string) error {
	return p.deleteDocument(ctx, IndexItemSubject, itemUID, "item_uid")
}

// indexerMessage is the upsert envelope: the indexer's own, narrowed so that
// Data cannot be an arbitrary domain type.
//
// Data is any rather than a single concrete type because this envelope serves
// two documents with two different hand-maintained wire shapes: a
// map[string]any for the checklist (projectionData) and a package-private
// struct for the item (itemProjectionWire). Both exist for the same reason —
// so that adding a field to FormationProjection or ItemProjection cannot
// silently publish it — an any-typed Data would accept either port struct
// directly, publishing every field it grows, including ones nobody decided
// the queue may show. The delete has no body to guard, so it has nothing to
// give up by using the shared indexerTypes.IndexerMessageEnvelope directly.
//
// Public is deliberately absent. Omitting it leaves the document
// access-controlled, and a formation checklist is never public — see
// formationAccessRelation.
type indexerMessage struct {
	Action         indexerConstants.MessageAction `json:"action"`
	Headers        map[string]string              `json:"headers"`
	Data           any                            `json:"data"`
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
//
// Only safe for a map[string]any value that is never marshaled on its own —
// a nil value still marshals to null inside a map, since a map has no
// struct-tag omitempty. projectionData accepts that trade for
// announcement_date; itemProjectionWire does not repeat it, using struct-tag
// omitempty instead. See itemProjectionWire's own comment.
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

// itemProjectionWire is the searchable body for one item, as a package-private
// struct with json tags rather than the hand-built map projectionData uses.
//
// Still the same allowlist projectionData is built to be: ItemProjection
// cannot reach the wire without an explicit field here, so a future addition
// to the domain struct still needs a conscious decision to publish it. What
// changes from a map is that omitempty is a struct tag the encoder honors
// directly — a nil map value marshals to null regardless of any per-field
// helper, which is the bug setIfNotEmpty existed to work around; a struct tag
// has no such gap. This also avoids boxing every field into `any` and the key
// sort encoding/json does for a map, on a path a sweep runs once per item per
// tick.
//
// Notes, skip reason and resolved-ref are deliberately absent — drawer-only
// detail, read from the checklist directly, never from this document.
type itemProjectionWire struct {
	ObjectID     string                      `json:"object_id"`
	FormationUID string                      `json:"formation_uid"`
	ProjectUID   string                      `json:"project_uid"`
	ItemKey      string                      `json:"item_key"`
	Title        string                      `json:"title"`
	Status       string                      `json:"status"`
	Gate         bool                        `json:"gate"`
	DueDate      string                      `json:"due_date,omitempty"`
	OwnerTeam    string                      `json:"owner_team,omitempty"`
	ActionLink   string                      `json:"action_link,omitempty"`
	Assignee     string                      `json:"assignee,omitempty"`
	SubItems     []itemProjectionSubItemWire `json:"sub_items"`
}

// itemProjectionSubItemWire is one entry of itemProjectionWire's nested,
// display-only sub-item summary. Nothing here is separately filterable.
type itemProjectionSubItemWire struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// newItemProjectionWire converts the domain projection to its wire shape.
func newItemProjectionWire(doc *port.ItemProjection) *itemProjectionWire {
	subItems := make([]itemProjectionSubItemWire, 0, len(doc.SubItems))
	for _, s := range doc.SubItems {
		subItems = append(subItems, itemProjectionSubItemWire{
			Key:    s.Key,
			Title:  s.Title,
			Status: s.Status,
		})
	}
	return &itemProjectionWire{
		ObjectID:     doc.ItemUID,
		FormationUID: doc.FormationUID,
		ProjectUID:   doc.ProjectUID,
		ItemKey:      doc.ItemKey,
		Title:        doc.Title,
		Status:       doc.Status,
		Gate:         doc.Gate,
		DueDate:      doc.DueDate,
		OwnerTeam:    doc.OwnerTeam,
		ActionLink:   doc.ActionLink,
		Assignee:     doc.Assignee,
		SubItems:     subItems,
	}
}

// itemParentRefs lets a caller filter items by either their project or their
// formation without a separate lookup. formation:<uid> parallels the
// checklist document's own object reference.
//
// Takes projectRef pre-built rather than a bare project UID: itemEnvelope
// already builds "project:"+ProjectUID for the access check, and this used to
// rebuild the identical string a second time.
func itemParentRefs(projectRef, formationUID string) []string {
	return []string{
		projectRef,
		formationRefPrefix + formationUID,
	}
}

// itemTags are the exact-match filters the Pending Actions query uses.
//
// assignee: is the one tag the Pending Actions query depends on — it is what
// turns "list every project, read every checklist" into one query.
// project_uid: and formation_uid: are included for parity with the checklist
// document's own tag set, not because any functional requirement here
// depends on them.
func itemTags(doc *port.ItemProjection) []string {
	tags := make([]string, 0, 3)
	tags = append(tags,
		tagProjectUID+doc.ProjectUID,
		tagFormationUID+doc.FormationUID,
	)
	if doc.Assignee != "" {
		tags = append(tags, tagAssignee+doc.Assignee)
	}
	return tags
}

// projectionTags are the exact-match filters the queue uses.
//
// Assignees are tagged because "Mine" is a filter rather than a full-text
// search, and a tag is what the generic search endpoint can match exactly. They
// are already in the document body, so tagging discloses nothing further.
func projectionTags(doc *port.FormationProjection) []string {
	tags := make([]string, 0, 4+len(doc.Assignees))
	tags = append(tags,
		tagProjectUID+doc.ProjectUID,
		"lifecycle:"+doc.Lifecycle,
	)
	if doc.ProjectSlug != "" {
		tags = append(tags, "project_slug:"+doc.ProjectSlug)
	}
	if doc.SubStage != "" {
		tags = append(tags, "sub_stage:"+doc.SubStage)
	}
	for _, a := range doc.Assignees {
		tags = append(tags, tagAssignee+a)
	}
	return tags
}
