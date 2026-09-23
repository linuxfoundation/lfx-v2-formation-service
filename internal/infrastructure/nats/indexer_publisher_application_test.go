// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

func sampleApplicationProjection() *port.ApplicationProjection {
	return &port.ApplicationProjection{
		ApplicationUID:    "8b1f6f2e-0000-4000-8000-000000000001",
		State:             "submitted",
		SubmitterUsername: "jdoe",
		SubmitterName:     "J Doe",
		SubmitterEmail:    "jdoe@example.org",
		ProjectName:       "A Proposed Project",
		CreatedAt:         "2026-09-01T00:00:00Z",
		UpdatedAt:         "2026-09-01T00:00:00Z",
		AccessRelation:    "viewer",
	}
}

// An application has no project, and the document must not claim indexing
// parentage.
//
// This is the assertion the whole type exists to protect. An application is a
// proposal to start a project, so it has no project_uid, slug or parent_refs.
// Its optional target_parent_uid remains private document data and does not
// scope search or authorization.
func TestPublishApplicationClaimsNoProjectParentage(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexApplicationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.PublishApplication(context.Background(), sampleApplicationProjection()); err != nil {
		t.Fatalf("PublishApplication() = %v, want no error", err)
	}

	raw := await()

	// Asserted on the bytes rather than on a decoded struct on purpose: a
	// field this test forgot to name is exactly the field that would leak,
	// and decoding into a known shape is what hides it.
	for _, forbidden := range []string{"project_uid", "project_slug"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("the application document carries %q; an application has no project, and a "+
				"parentage claim here resolves its access through a project the applicant has "+
				"nothing to do with\npayload: %s", forbidden, raw)
		}
	}

	var envelope struct {
		IndexingConfig struct {
			ObjectType string `json:"object_type"`
			ObjectID   string `json:"object_id"`
			ParentRefs []any  `json:"parent_refs"`
		} `json:"indexing_config"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("Unmarshal() = %v, want no error\npayload: %s", err, raw)
	}
	if len(envelope.IndexingConfig.ParentRefs) != 0 {
		t.Errorf("parent_refs = %v, want empty — an application answers to nothing",
			envelope.IndexingConfig.ParentRefs)
	}
	if envelope.IndexingConfig.ObjectID != sampleApplicationProjection().ApplicationUID {
		t.Errorf("object_id = %q, want the application uid", envelope.IndexingConfig.ObjectID)
	}
}

func TestPublishApplicationCarriesPrivateReviewDetails(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexApplicationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleApplicationProjection()
	wantPayload := map[string]any{"project_name": "A Proposed Project", "mission": "Build things"}
	wantTarget := "8b1f6f2e-0000-4000-8000-000000000002"
	doc.Payload = wantPayload
	doc.TargetParentUID = &wantTarget

	if err := publisher.PublishApplication(context.Background(), doc); err != nil {
		t.Fatalf("PublishApplication() = %v, want no error", err)
	}

	var envelope struct {
		Data struct {
			Application     map[string]any `json:"application"`
			TargetParentUID *string        `json:"target_parent_uid"`
		} `json:"data"`
	}
	raw := await()
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("Unmarshal() = %v, want no error\npayload: %s", err, raw)
	}
	if !reflect.DeepEqual(envelope.Data.Application, wantPayload) {
		t.Errorf("application = %#v, want %#v", envelope.Data.Application, wantPayload)
	}
	if envelope.Data.TargetParentUID == nil || *envelope.Data.TargetParentUID != wantTarget {
		t.Errorf("target_parent_uid = %v, want %q", envelope.Data.TargetParentUID, wantTarget)
	}
}

// The access block must name the application itself, checked through a
// relation that is not the public wildcard.
func TestPublishApplicationChecksAccessOnTheApplicationItself(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexApplicationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleApplicationProjection()
	if err := publisher.PublishApplication(context.Background(), doc); err != nil {
		t.Fatalf("PublishApplication() = %v, want no error", err)
	}

	raw := await()

	var envelope struct {
		IndexingConfig struct {
			AccessCheckObject   string `json:"access_check_object"`
			AccessCheckRelation string `json:"access_check_relation"`
			Public              bool   `json:"public"`
		} `json:"indexing_config"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("Unmarshal() = %v, want no error\npayload: %s", err, raw)
	}

	wantObject := "project_application:" + doc.ApplicationUID
	if envelope.IndexingConfig.AccessCheckObject != wantObject {
		t.Errorf("access_check_object = %q, want %q", envelope.IndexingConfig.AccessCheckObject, wantObject)
	}
	if envelope.IndexingConfig.AccessCheckRelation != "viewer" {
		t.Errorf("access_check_relation = %q, want viewer", envelope.IndexingConfig.AccessCheckRelation)
	}
	if envelope.IndexingConfig.Public {
		t.Error("public = true; an unannounced proposal carries a named person's email and is " +
			"never world-readable")
	}
}
