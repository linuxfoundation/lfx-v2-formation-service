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
