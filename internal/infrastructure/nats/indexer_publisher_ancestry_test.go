// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// The query service compiles its parent filter to a term match over
// parent_refs, so publishing every generation is what makes one query resolve
// a foundation's descendants at any depth. These assert the emitted array
// rather than the projection struct, because the array is the contract.

// refsFromEnvelope pulls parent_refs out of a published envelope, returning
// the envelope too so a test can assert the body was left alone.
func refsFromEnvelope(t *testing.T, raw []byte) ([]string, map[string]any) {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decoding the envelope = %v", err)
	}
	config, ok := envelope["indexing_config"].(map[string]any)
	if !ok {
		t.Fatalf("envelope carries no indexing_config: %v", envelope)
	}
	rawRefs, _ := config["parent_refs"].([]any)
	refs := make([]string, 0, len(rawRefs))
	for _, r := range rawRefs {
		if s, ok := r.(string); ok {
			refs = append(refs, s)
		}
	}
	return refs, envelope
}

func TestPublishFormationCarriesTheWholeChainInGenerationOrder(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleProjection()
	doc.ParentUID = "parent-1"
	doc.AncestorUIDs = []string{"project-1", "parent-1", "foundation-1", "root-1"}

	if err := publisher.PublishFormation(context.Background(), doc); err != nil {
		t.Fatalf("PublishFormation() = %v, want no error", err)
	}

	refs, _ := refsFromEnvelope(t, await())
	want := []string{"project:project-1", "project:parent-1", "project:foundation-1", "project:root-1"}
	if len(refs) != len(want) {
		t.Fatalf("parent_refs = %v, want %v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("parent_refs = %v, want %v — nearest first", refs, want)
		}
	}
}

// A duplicate would put the same row into one foundation's queue twice.
// Resolution will not produce one, which is exactly why the publisher should
// not rely on that.
func TestPublishFormationCollapsesDuplicateAncestors(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleProjection()
	doc.AncestorUIDs = []string{"project-1", "foundation-1", "project-1", "foundation-1", ""}

	if err := publisher.PublishFormation(context.Background(), doc); err != nil {
		t.Fatalf("PublishFormation() = %v, want no error", err)
	}

	refs, _ := refsFromEnvelope(t, await())
	seen := map[string]int{}
	for _, r := range refs {
		seen[r]++
	}
	for ref, n := range seen {
		if n > 1 {
			t.Errorf("parent_refs carries %q %d times, want once", ref, n)
		}
	}
	if len(refs) != 2 {
		t.Errorf("parent_refs = %v, want two distinct entries and no empty", refs)
	}
}

// The Type column reads the direct parent off the document body and must keep
// seeing exactly one project there. Widening the refs must not disturb it.
func TestTheDirectParentInTheBodyIsUnchangedByTheChain(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleProjection()
	doc.ParentUID = "parent-1"
	doc.AncestorUIDs = []string{"project-1", "parent-1", "foundation-1", "root-1"}

	if err := publisher.PublishFormation(context.Background(), doc); err != nil {
		t.Fatalf("PublishFormation() = %v, want no error", err)
	}

	_, envelope := refsFromEnvelope(t, await())
	body, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope carries no data body: %v", envelope)
	}
	if got := body["parent_uid"]; got != "parent-1" {
		t.Errorf("data.parent_uid = %v, want parent-1 — one project, not the chain", got)
	}
}

// No parent tag is added. Tags are the aggregatable dimension and nothing asks
// to group by foundation, so a tag here would be surface with no consumer.
func TestTheChainAddsNoParentTag(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleProjection()
	doc.AncestorUIDs = []string{"project-1", "foundation-1", "root-1"}

	if err := publisher.PublishFormation(context.Background(), doc); err != nil {
		t.Fatalf("PublishFormation() = %v, want no error", err)
	}

	_, envelope := refsFromEnvelope(t, await())
	config, _ := envelope["indexing_config"].(map[string]any)
	rawTags, _ := config["tags"].([]any)
	for _, raw := range rawTags {
		tag, _ := raw.(string)
		for _, forbidden := range []string{"parent", "ancestor", "foundation_uid"} {
			if len(tag) >= len(forbidden) && tag[:len(forbidden)] == forbidden {
				t.Errorf("tags carry %q; the chain belongs in parent_refs, not in tags", tag)
			}
		}
	}
}

// The widening is only safe if the behaviour it widens still works. A
// formation one level under a foundation must still be found by scoping to
// that foundation, exactly as it is today.
func TestADirectParentIsStillMatchedByItsFoundation(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleProjection()
	doc.ParentUID = "foundation-1"
	doc.AncestorUIDs = []string{"project-1", "foundation-1"}

	if err := publisher.PublishFormation(context.Background(), doc); err != nil {
		t.Fatalf("PublishFormation() = %v, want no error", err)
	}

	refs, _ := refsFromEnvelope(t, await())
	found := false
	for _, r := range refs {
		if r == "project:foundation-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("parent_refs = %v, missing the direct parent — direct-parent scoping would regress", refs)
	}
}

// A document built without a chain must not be less scoped than it used to be.
// This is the shape any caller that has not been updated still produces.
func TestWithoutAChainTheDirectParentIsStillEmitted(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexFormationSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	doc := sampleProjection()
	doc.ParentUID = "parent-1"
	doc.AncestorUIDs = nil

	if err := publisher.PublishFormation(context.Background(), doc); err != nil {
		t.Fatalf("PublishFormation() = %v, want no error", err)
	}

	refs, _ := refsFromEnvelope(t, await())
	if len(refs) != 1 || refs[0] != "project:parent-1" {
		t.Errorf("parent_refs = %v, want just the direct parent", refs)
	}
}

// Item documents are deliberately left alone. Their only specified consumer
// narrows by assignment rather than by foundation, so they gain nothing from a
// chain — and this is asserted rather than omitted so the two document types
// are not "made consistent" later without a consumer asking for it.
func TestItemDocumentsCarryNoAncestry(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, IndexItemSubject)
	publisher := NewIndexerPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.PublishItem(context.Background(), sampleItemProjection()); err != nil {
		t.Fatalf("PublishItem() = %v, want no error", err)
	}

	refs, _ := refsFromEnvelope(t, await())
	want := map[string]bool{
		"project:project-1":                    true,
		"formation:01JQ0000000000000000000000": true,
	}
	if len(refs) != len(want) {
		t.Fatalf("item parent_refs = %v, want exactly the project and formation refs", refs)
	}
	for _, r := range refs {
		if !want[r] {
			t.Errorf("item parent_refs carries %q — items gained ancestry they have no consumer for", r)
		}
	}
}
