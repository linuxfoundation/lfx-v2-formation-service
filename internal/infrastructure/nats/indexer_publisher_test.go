// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// captureOn subscribes to a subject and hands back a function that waits for one
// message, so a publish test asserts on the bytes that actually crossed the
// connection rather than on a struct that was never serialised.
func captureOn(t *testing.T, url, subject string) func() []byte {
	t.Helper()

	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("Connect() = %v, want no error", err)
	}
	t.Cleanup(conn.Close)

	received := make(chan []byte, 4)
	sub, err := conn.Subscribe(subject, func(msg *nats.Msg) {
		received <- msg.Data
	})
	if err != nil {
		t.Fatalf("Subscribe(%s) = %v, want no error", subject, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("Flush() = %v, want no error", err)
	}

	return func() []byte {
		t.Helper()
		select {
		case data := <-received:
			return data
		case <-time.After(3 * time.Second):
			t.Fatalf("no message on %s", subject)
			return nil
		}
	}
}

func sampleItemProjection() *port.ItemProjection {
	return &port.ItemProjection{
		ItemUID:      "01JQ0000000000000000000001",
		FormationUID: "01JQ0000000000000000000000",
		ProjectUID:   "project-1",
		ItemKey:      "create_mailing_list",
		Title:        "Create mailing list",
		Status:       "in_progress",
		Gate:         false,
		DueDate:      "2026-10-01",
		OwnerTeam:    "it",
		ActionLink:   "https://groups.io/g/create",
		Assignee:     "jdoe",
		SubItems: []port.ItemProjectionSubItem{
			{Key: "groupsio-request", Title: "Request Groups.io space", Status: "done"},
		},
		AccessRelation: "auditor",
	}
}

func sampleProjection() *port.FormationProjection {
	return &port.FormationProjection{
		FormationUID:      "01JQ0000000000000000000000",
		ProjectUID:        "project-1",
		ProjectName:       "A Project",
		ProjectSlug:       "a-project",
		ParentUID:         "parent-1",
		IsFoundation:      true,
		SubStage:          "Formation - Engaged",
		Lifecycle:         "live",
		AnnouncementDate:  "2026-12-01",
		GatesCleared:      true,
		IsActivating:      true,
		Blocked:           1,
		BlockedItemTitles: []string{"Charter agreed"},
		Assignees:         []string{"assignee-one"},
		AccessRelation:    "auditor",
	}
}

// The envelope has to satisfy the indexer's own validation, which is stricter
// than it looks: a V2 message with no authorization header is refused outright,
// and this service never has a user behind a publish.
func TestPublishFormationCarriesAnAuthorizationHeader(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.PublishFormation(context.Background(), sampleProjection()); err != nil {
		t.Fatalf("PublishFormation() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}

	headers, ok := envelope["headers"].(map[string]any)
	if !ok {
		t.Fatalf("envelope carries no headers; the indexer refuses it: %v", envelope)
	}
	if got := headers["authorization"]; got != serviceAccountBearer {
		t.Errorf("authorization = %v, want the service-account bearer %q", got, serviceAccountBearer)
	}
}

// Always "updated", never "created". The indexer upserts on either, and the
// distinction only decides whether it stamps created_by or updated_by — so
// claiming creation on a republish rewrites the creation record of a document
// that already existed, which every sweep after the first would do.
func TestPublishFormationAlwaysUpdates(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.PublishFormation(context.Background(), sampleProjection()); err != nil {
		t.Fatalf("PublishFormation() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}
	if got := envelope["action"]; got != "updated" {
		t.Errorf("action = %v, want updated", got)
	}
}

// The access declaration is carried from the document rather than chosen here,
// and this asserts it survives serialisation — the one place a correct decision
// in the domain could still be published wrongly.
func TestPublishFormationCarriesTheDocumentsAccessRelation(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleProjection()
	if err := publisher.PublishFormation(context.Background(), doc); err != nil {
		t.Fatalf("PublishFormation() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}
	config, ok := envelope["indexing_config"].(map[string]any)
	if !ok {
		t.Fatalf("envelope carries no indexing_config, so access is undeclared: %v", envelope)
	}

	if got := config["access_check_relation"]; got != "auditor" {
		t.Errorf("access_check_relation = %v, want auditor", got)
	}
	if got, want := config["access_check_object"], "project:project-1"; got != want {
		t.Errorf("access_check_object = %v, want %v", got, want)
	}
	if got := config["history_check_relation"]; got != "auditor" {
		t.Errorf("history_check_relation = %v, want auditor", got)
	}
	if got := config["object_id"]; got != doc.FormationUID {
		t.Errorf("object_id = %v, want the formation UID %v", got, doc.FormationUID)
	}

	// public: true would make the relation decorative — the indexer would serve
	// the document without checking it.
	if value, present := config["public"]; present {
		t.Errorf("public is present as %v; it must be omitted so the relation decides", value)
	}
}

// A document that declares no relation is refused rather than defaulted. The
// obvious default is the relation every other project document uses, and that
// relation is viewer.
func TestPublishFormationRefusesADocumentWithNoAccessRelation(t *testing.T) {
	url := startTestNATSServer(t)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleProjection()
	doc.AccessRelation = ""

	if err := publisher.PublishFormation(context.Background(), doc); err == nil {
		t.Fatal("PublishFormation() with no access relation = nil, want an error rather than " +
			"an ungated document")
	}
}

func TestPublishFormationRefusesADocumentWithNoProject(t *testing.T) {
	url := startTestNATSServer(t)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleProjection()
	doc.ProjectUID = ""

	if err := publisher.PublishFormation(context.Background(), doc); err == nil {
		t.Fatal("PublishFormation() with no project_uid = nil, want an error — the access check " +
			"is built from it")
	}
}

// The indexer refuses indexing_config.object_id when it is empty. Nothing on
// this side would notice, because the publish expects no reply, so the row would
// simply be absent from the queue.
func TestPublishFormationRefusesADocumentWithNoFormationUID(t *testing.T) {
	url := startTestNATSServer(t)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleProjection()
	doc.FormationUID = ""

	if err := publisher.PublishFormation(context.Background(), doc); err == nil {
		t.Fatal("PublishFormation() with no formation_uid = nil, want an error — it becomes " +
			"object_id, which the indexer requires")
	}
}

// The queue sorts undated rows last. Sending an empty string would make them
// sort first under an ascending sort, so the field is omitted instead and the
// search's missing-last ordering applies.
func TestAnUnsetAnnouncementDateIsOmittedRatherThanEmpty(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleProjection()
	doc.AnnouncementDate = ""
	if err := publisher.PublishFormation(context.Background(), doc); err != nil {
		t.Fatalf("PublishFormation() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}
	data, _ := envelope["data"].(map[string]any)
	if value, present := data["announcement_date"]; present && value != nil {
		t.Errorf("announcement_date = %v, want it absent so undated rows sort last", value)
	}
}

// The published body is the list of what the queue may show. A new field arriving
// here should be a decision, so the key set is asserted rather than sampled.
func TestPublishedDataIsTheAgreedFieldSet(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.PublishFormation(context.Background(), sampleProjection()); err != nil {
		t.Fatalf("PublishFormation() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope carries no data object: %v", envelope)
	}

	want := map[string]bool{
		"formation_uid": true, "project_uid": true, "project_name": true,
		"project_slug": true, "is_foundation": true, "parent_uid": true,
		"sub_stage": true, "lifecycle": true, "gates_cleared": true,
		"is_activating": true, "announcement_date": true, "progress": true,
		"blocked_item_titles": true, "assignees": true,
	}
	for key := range data {
		if !want[key] {
			t.Errorf("unexpected published field %q — decide whether the queue may show it", key)
		}
	}
	for key := range want {
		if _, present := data[key]; !present {
			t.Errorf("field %q is no longer published; the queue reads it", key)
		}
	}

	// Items are never published: their notes and skip reasons are free text about
	// partners and negotiations, and the queue displays none of it.
	for _, forbidden := range []string{"items", "sections", "note", "skip_reason", "activity"} {
		if _, present := data[forbidden]; present {
			t.Errorf("field %q must not be published", forbidden)
		}
	}

	progress, ok := data["progress"].(map[string]any)
	if !ok {
		t.Fatalf("progress is not an object: %v", data["progress"])
	}
	for _, key := range []string{
		"not_started", "in_progress", "blocked", "awaiting_acceptance", "done", "skipped",
	} {
		if _, present := progress[key]; !present {
			t.Errorf("progress is missing %q; the queue sorts on all six", key)
		}
	}
}

// "Mine" is an exact-match filter rather than a search, so the assignees have to
// be tagged as well as carried in the body.
func TestAssigneesAreTaggedForTheMineFilter(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.PublishFormation(context.Background(), sampleProjection()); err != nil {
		t.Fatalf("PublishFormation() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}
	config, _ := envelope["indexing_config"].(map[string]any)
	rawTags, _ := config["tags"].([]any)

	tags := make(map[string]bool, len(rawTags))
	for _, tag := range rawTags {
		if s, ok := tag.(string); ok {
			tags[s] = true
		}
	}
	for _, want := range []string{
		"assignee:assignee-one", "project_uid:project-1", "lifecycle:live",
		"project_slug:a-project", "sub_stage:Formation - Engaged",
	} {
		if !tags[want] {
			t.Errorf("tags are missing %q; got %v", want, rawTags)
		}
	}
}

// The object type the indexer files this under comes from the subject alone —
// it cuts the lfx.index. prefix and uses the remainder, with no registry to
// add a type to. So this constant is the whole declaration, and a typo would
// index a type nothing searches.
func TestTheSubjectNamesTheFormationObjectType(t *testing.T) {
	if IndexFormationSubject != "lfx.index.formation" {
		t.Errorf("IndexFormationSubject = %q, want lfx.index.formation — the indexer derives "+
			"the object type from what follows lfx.index.", IndexFormationSubject)
	}
}

// The one place this envelope departs from every other message the service
// sends, and the departure is invisible at runtime: the indexer reads the object
// ID straight out of data for a delete, where it decodes a body for everything
// else. Encoding the UID the way the upsert encodes its own would delete
// nothing — and report nothing, because the publish is fire-and-forget. That is
// exactly how an orphaned row survives a repair job that looked like it worked,
// so the shape is pinned here rather than trusted.
func TestDeleteFormationSendsTheUIDAsABareString(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	const formationUID = "01JQ0000000000000000000000"
	if err := publisher.DeleteFormation(context.Background(), formationUID); err != nil {
		t.Fatalf("DeleteFormation() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}

	if got := envelope["action"]; got != "deleted" {
		t.Errorf("action = %v, want deleted", got)
	}
	got, ok := envelope["data"].(string)
	if !ok {
		t.Fatalf("data = %#v, want the UID as a bare string; an object here deletes nothing", envelope["data"])
	}
	if got != formationUID {
		t.Errorf("data = %q, want %q", got, formationUID)
	}
	// Nothing to gate or sort once the document is gone.
	if _, present := envelope["indexing_config"]; present {
		t.Errorf("envelope carries indexing_config on a delete: %v", envelope)
	}
	// The indexer refuses a V2 message with no authorization header, delete
	// included.
	headers, ok := envelope["headers"].(map[string]any)
	if !ok || headers["authorization"] != serviceAccountBearer {
		t.Errorf("headers = %v, want the service-account bearer", envelope["headers"])
	}
}

// The indexer validates the object ID and refuses an empty one, but that refusal
// arrives here as silence. Caught where it is attributable instead.
func TestDeleteFormationRefusesAnEmptyUID(t *testing.T) {
	url := startTestNATSServer(t)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.DeleteFormation(context.Background(), ""); err == nil {
		t.Error("DeleteFormation(\"\") = nil, want an error rather than a publish nothing acts on")
	}
}

// The object type the indexer files this under comes from the subject alone,
// the same way IndexFormationSubject's does — no registration step.
func TestTheSubjectNamesTheFormationItemObjectType(t *testing.T) {
	if IndexItemSubject != "lfx.index.formation_item" {
		t.Errorf("IndexItemSubject = %q, want lfx.index.formation_item — the indexer derives "+
			"the object type from what follows lfx.index.", IndexItemSubject)
	}
}

// Always "updated", never "created" — same reasoning as PublishFormation: the
// reconcile sweep cannot tell a genuine first publish from a republish of an
// item that already exists.
func TestPublishItemAlwaysUpdates(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexItemSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.PublishItem(context.Background(), sampleItemProjection()); err != nil {
		t.Fatalf("PublishItem() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}
	if got := envelope["action"]; got != "updated" {
		t.Errorf("action = %v, want updated", got)
	}
}

// access_check_object is built from the item's own ProjectUID, and the
// relation is whatever the projection declared — never a value this package
// chose for itself.
func TestPublishItemCarriesTheDocumentsAccessRelation(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexItemSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleItemProjection()
	if err := publisher.PublishItem(context.Background(), doc); err != nil {
		t.Fatalf("PublishItem() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}
	config, ok := envelope["indexing_config"].(map[string]any)
	if !ok {
		t.Fatalf("envelope carries no indexing_config, so access is undeclared: %v", envelope)
	}

	if got := config["access_check_relation"]; got != "auditor" {
		t.Errorf("access_check_relation = %v, want auditor", got)
	}
	if got, want := config["access_check_object"], "project:project-1"; got != want {
		t.Errorf("access_check_object = %v, want %v", got, want)
	}
	if got := config["history_check_relation"]; got != "auditor" {
		t.Errorf("history_check_relation = %v, want auditor", got)
	}
	if got := config["object_id"]; got != doc.ItemUID {
		t.Errorf("object_id = %v, want the item UID %v", got, doc.ItemUID)
	}
	if value, present := config["public"]; present {
		t.Errorf("public is present as %v; it must be omitted so the relation decides", value)
	}
}

// parent_refs lets a caller filter items by either their project or their
// formation without a separate lookup.
func TestPublishItemCarriesBothParentRefs(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexItemSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.PublishItem(context.Background(), sampleItemProjection()); err != nil {
		t.Fatalf("PublishItem() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}
	config, _ := envelope["indexing_config"].(map[string]any)
	rawRefs, _ := config["parent_refs"].([]any)
	refs := make(map[string]bool, len(rawRefs))
	for _, r := range rawRefs {
		if s, ok := r.(string); ok {
			refs[s] = true
		}
	}
	for _, want := range []string{"project:project-1", "formation:01JQ0000000000000000000000"} {
		if !refs[want] {
			t.Errorf("parent_refs is missing %q; got %v", want, rawRefs)
		}
	}
}

// tags carry project_uid:, formation_uid:, and assignee: only when the item
// has one — the one tag the Pending Actions query depends on to turn "list
// every project, read every checklist" into one query.
func TestPublishItemTagsIncludeAssigneeOnlyWhenSet(t *testing.T) {
	url := startTestNATSServer(t)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	for _, tc := range []struct {
		name         string
		assignee     string
		wantAssignee bool
	}{
		{"assigned", "jdoe", true},
		{"unassigned", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			await := captureOn(t, url, IndexItemSubject)
			doc := sampleItemProjection()
			doc.Assignee = tc.assignee
			if err := publisher.PublishItem(context.Background(), doc); err != nil {
				t.Fatalf("PublishItem() = %v, want no error", err)
			}

			var envelope map[string]any
			if err := json.Unmarshal(await(), &envelope); err != nil {
				t.Fatalf("decoding the envelope = %v", err)
			}
			config, _ := envelope["indexing_config"].(map[string]any)
			rawTags, _ := config["tags"].([]any)
			tags := make(map[string]bool, len(rawTags))
			for _, tag := range rawTags {
				if s, ok := tag.(string); ok {
					tags[s] = true
				}
			}
			if !tags["project_uid:project-1"] || !tags["formation_uid:01JQ0000000000000000000000"] {
				t.Errorf("tags are missing project_uid:/formation_uid:; got %v", rawTags)
			}
			hasAssigneeTag := tags["assignee:jdoe"]
			if hasAssigneeTag != tc.wantAssignee {
				t.Errorf("assignee tag present = %v, want %v; got %v", hasAssigneeTag, tc.wantAssignee, rawTags)
			}
		})
	}
}

// The published body is the list of what a Pending Actions row may show.
// Notes, skip reason and resolved-ref must never appear — they are
// drawer-only detail, read from the checklist directly.
func TestPublishedItemDataExcludesDrawerOnlyFields(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexItemSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.PublishItem(context.Background(), sampleItemProjection()); err != nil {
		t.Fatalf("PublishItem() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope carries no data object: %v", envelope)
	}

	want := map[string]bool{
		"object_id": true, "formation_uid": true, "project_uid": true, "item_key": true,
		"title": true, "status": true, "gate": true, "due_date": true, "owner_team": true,
		"action_link": true, "assignee": true, "sub_items": true,
	}
	for key := range data {
		if !want[key] {
			t.Errorf("unexpected published field %q — decide whether the row may show it", key)
		}
	}
	for key := range want {
		if _, present := data[key]; !present {
			t.Errorf("field %q is no longer published; the row reads it", key)
		}
	}

	for _, forbidden := range []string{"note", "skip_reason", "resolved_ref", "evidence_link"} {
		if _, present := data[forbidden]; present {
			t.Errorf("field %q must not be published — it is drawer-only detail", forbidden)
		}
	}
}

// due_date, owner_team, action_link and assignee are omitted rather than sent
// empty, matching the checklist projection's own omitEmpty convention.
func TestUnsetOptionalItemFieldsAreOmittedRatherThanEmpty(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexItemSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleItemProjection()
	doc.DueDate = ""
	doc.OwnerTeam = ""
	doc.ActionLink = ""
	doc.Assignee = ""
	if err := publisher.PublishItem(context.Background(), doc); err != nil {
		t.Fatalf("PublishItem() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}
	data, _ := envelope["data"].(map[string]any)
	for _, field := range []string{"due_date", "owner_team", "action_link", "assignee"} {
		// The key itself must be absent, not present with a null value: a
		// map has no struct-tag omitempty, so a nil map value still
		// marshals as `"field":null` rather than dropping the key.
		if value, present := data[field]; present {
			t.Errorf("%s key present (value %v), want the key absent when unset", field, value)
		}
	}
}

// PublishItem refuses a document missing any of the four fields an indexed
// document cannot be published without — matching PublishFormation's own
// refuse-rather-than-default checks. A document that declares no access
// relation is refused rather than defaulted, for the same reason
// PublishFormation refuses one: this document can never ship with a
// silently-defaulted access relation.
func TestPublishItemRefusesAnIncompleteDocument(t *testing.T) {
	url := startTestNATSServer(t)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	tests := []struct {
		name   string
		mutate func(*port.ItemProjection)
	}{
		{"no access relation", func(d *port.ItemProjection) { d.AccessRelation = "" }},
		{"no project_uid", func(d *port.ItemProjection) { d.ProjectUID = "" }},
		{"no formation_uid", func(d *port.ItemProjection) { d.FormationUID = "" }},
		{"no item_uid", func(d *port.ItemProjection) { d.ItemUID = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := sampleItemProjection()
			tt.mutate(doc)

			if err := publisher.PublishItem(context.Background(), doc); err == nil {
				t.Fatalf("PublishItem() with %s = nil, want an error rather than an "+
					"incomplete document reaching the wire", tt.name)
			}
		})
	}
}

// captureN subscribes to a subject and hands back a function that waits for
// exactly n messages, for a test asserting on a batch publish rather than a
// single one.
func captureN(t *testing.T, url, subject string, n int) func() [][]byte {
	t.Helper()

	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("Connect() = %v, want no error", err)
	}
	t.Cleanup(conn.Close)

	received := make(chan []byte, n)
	sub, err := conn.Subscribe(subject, func(msg *nats.Msg) {
		received <- msg.Data
	})
	if err != nil {
		t.Fatalf("Subscribe(%s) = %v, want no error", subject, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("Flush() = %v, want no error", err)
	}

	return func() [][]byte {
		t.Helper()
		out := make([][]byte, 0, n)
		for range n {
			select {
			case data := <-received:
				out = append(out, data)
			case <-time.After(3 * time.Second):
				t.Fatalf("only got %d of %d messages on %s", len(out), n, subject)
			}
		}
		return out
	}
}

// PublishItems exists so a checklist of N items costs one flush rather than
// N (finding: per-item publish costs one NATS round trip per item). This
// asserts the batch actually reaches the wire as N separate messages with the
// same shape a loop of PublishItem calls would produce — the flush is an
// internal detail the messages received must not depend on.
func TestPublishItemsSendsOneMessagePerDocument(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureN(t, url, IndexItemSubject, 2)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	first := sampleItemProjection()
	second := sampleItemProjection()
	second.ItemUID = "01JQ0000000000000000000002"
	second.Assignee = ""

	if err := publisher.PublishItems(context.Background(), []*port.ItemProjection{first, second}); err != nil {
		t.Fatalf("PublishItems() = %v, want no error", err)
	}

	messages := await()
	seen := map[string]bool{}
	for _, raw := range messages {
		var envelope map[string]any
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("decoding a message = %v", err)
		}
		data, _ := envelope["data"].(map[string]any)
		seen[data["object_id"].(string)] = true
	}
	for _, want := range []string{first.ItemUID, second.ItemUID} {
		if !seen[want] {
			t.Errorf("PublishItems() did not publish item %s", want)
		}
	}
}

// A batch where every document is invalid publishes nothing and is worth the
// caller knowing about; a batch where only some are is the ordinary
// best-effort case the sweep already tolerates per item, and reports no error
// so the valid documents' publish is not treated as a failure.
func TestPublishItemsToleratesSomeInvalidDocuments(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureN(t, url, IndexItemSubject, 1)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	valid := sampleItemProjection()
	invalid := sampleItemProjection()
	invalid.ItemUID = ""

	if err := publisher.PublishItems(context.Background(), []*port.ItemProjection{invalid, valid}); err != nil {
		t.Fatalf("PublishItems() with one invalid document among two = %v, want no error "+
			"— the valid one still landed", err)
	}
	await()
}

func TestPublishItemsRefusesWhenEveryDocumentIsInvalid(t *testing.T) {
	url := startTestNATSServer(t)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	invalid := sampleItemProjection()
	invalid.ItemUID = ""

	if err := publisher.PublishItems(context.Background(), []*port.ItemProjection{invalid, invalid}); err == nil {
		t.Fatal("PublishItems() with every document invalid = nil, want an error — " +
			"nothing landed for this checklist")
	}
}

// DeleteItem mirrors DeleteFormation's envelope exactly.
func TestDeleteItemSendsTheUIDAsABareString(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexItemSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	const itemUID = "01JQ0000000000000000000001"
	if err := publisher.DeleteItem(context.Background(), itemUID); err != nil {
		t.Fatalf("DeleteItem() = %v, want no error", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(await(), &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}

	if got := envelope["action"]; got != "deleted" {
		t.Errorf("action = %v, want deleted", got)
	}
	got, ok := envelope["data"].(string)
	if !ok {
		t.Fatalf("data = %#v, want the UID as a bare string; an object here deletes nothing", envelope["data"])
	}
	if got != itemUID {
		t.Errorf("data = %q, want %q", got, itemUID)
	}
	if _, present := envelope["indexing_config"]; present {
		t.Errorf("envelope carries indexing_config on a delete: %v", envelope)
	}
	headers, ok := envelope["headers"].(map[string]any)
	if !ok || headers["authorization"] != serviceAccountBearer {
		t.Errorf("headers = %v, want the service-account bearer", envelope["headers"])
	}
}

func TestDeleteItemRefusesAnEmptyUID(t *testing.T) {
	url := startTestNATSServer(t)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.DeleteItem(context.Background(), ""); err == nil {
		t.Error("DeleteItem(\"\") = nil, want an error rather than a publish nothing acts on")
	}
}
