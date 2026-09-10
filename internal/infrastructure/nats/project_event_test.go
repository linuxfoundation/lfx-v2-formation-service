// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import "testing"

// A document as the dev index actually holds it.
//
// Copied from a real project event rather than composed from the publisher's
// struct tags, because the two can differ: is_foundation is omitted when false,
// parent_uid and parent_refs say the same thing in different forms, and no
// amount of reading the publisher would have shown which fields survive to the
// index. Trimmed of the fields this service does not read.
const realProjectEvent = `{
  "document_id": "project:78738b77-132a-42fd-8fd7-d0f0a18309ff",
  "object_id": "78738b77-132a-42fd-8fd7-d0f0a18309ff",
  "object_type": "project",
  "action": "updated",
  "body": {
    "object_type": "project",
    "object_id": "78738b77-132a-42fd-8fd7-d0f0a18309ff",
    "parent_refs": ["project:a88f2a5a-e0f3-4f1c-87d4-731ac79f2a19"],
    "public": true,
    "data": {
      "uid": "78738b77-132a-42fd-8fd7-d0f0a18309ff",
      "slug": "mscs",
      "stage": "Formation - Exploratory",
      "is_foundation": false,
      "parent_uid": "a88f2a5a-e0f3-4f1c-87d4-731ac79f2a19"
    }
  },
  "timestamp": "2026-09-10T09:41:02.117Z"
}`

func TestDecodeReadsARealProjectEvent(t *testing.T) {
	ref, err := DecodeProjectEvent([]byte(realProjectEvent))
	if err != nil {
		t.Fatalf("DecodeProjectEvent() = %v, want no error", err)
	}

	if ref.UID != "78738b77-132a-42fd-8fd7-d0f0a18309ff" {
		t.Errorf("uid = %q", ref.UID)
	}
	if ref.Slug != "mscs" {
		t.Errorf("slug = %q, want mscs", ref.Slug)
	}
	if ref.SubStage != "Formation - Exploratory" {
		t.Errorf("stage = %q, want Formation - Exploratory", ref.SubStage)
	}
	if ref.ParentUID != "a88f2a5a-e0f3-4f1c-87d4-731ac79f2a19" {
		t.Errorf("parent = %q", ref.ParentUID)
	}
	if ref.IsFoundation {
		t.Error("is_foundation = true, want false")
	}
}

// An absent stage decodes to an empty one rather than to an error.
//
// The decoder does not get to decide what a missing stage means. It is the
// majority case in the index, and reading it as "not forming" would freeze live
// checklists — a decision with consequences, which belongs where it can be seen
// rather than inside a parser.
func TestDecodeLeavesAnAbsentStageEmpty(t *testing.T) {
	ref, err := DecodeProjectEvent([]byte(
		`{"object_id":"p1","object_type":"project","body":{"data":{"uid":"p1","slug":"p"}}}`))
	if err != nil {
		t.Fatalf("DecodeProjectEvent() = %v, want no error", err)
	}
	if ref.SubStage != "" {
		t.Errorf("stage = %q, want empty", ref.SubStage)
	}
	if ref.UID != "p1" {
		t.Errorf("uid = %q, want p1", ref.UID)
	}
}

// parent_refs is the fallback when the body states no parent_uid, and the
// object-type prefix comes off.
func TestDecodeFallsBackToParentRefs(t *testing.T) {
	ref, err := DecodeProjectEvent([]byte(
		`{"object_id":"p1","object_type":"project","body":{"parent_refs":["project:par-1"],"data":{"uid":"p1"}}}`))
	if err != nil {
		t.Fatalf("DecodeProjectEvent() = %v, want no error", err)
	}
	if ref.ParentUID != "par-1" {
		t.Errorf("parent = %q, want par-1 with the prefix removed", ref.ParentUID)
	}
}

func TestDecodeRefusesWhatItCannotUse(t *testing.T) {
	for name, payload := range map[string]string{
		"not json":          `{`,
		"another type":      `{"object_type":"committee","body":{"data":{"uid":"c1"}}}`,
		"no body":           `{"object_id":"p1","object_type":"project"}`,
		"no project named":  `{"object_type":"project","body":{"data":{"slug":"anonymous"}}}`,
		"data of no fields": `{"object_type":"project","body":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeProjectEvent([]byte(payload)); err == nil {
				t.Errorf("DecodeProjectEvent(%s) = nil, want an error", payload)
			}
		})
	}
}

// A field of the wrong type is absent rather than fatal.
//
// The producer is another service and another module, so this side cannot be
// compiled against its shape. Treating a surprise as an absence keeps one
// upstream change from turning every event into a dropped one, and the sweep
// still repairs whatever the thinner ref failed to do.
func TestDecodeTreatsAWrongTypeAsAbsent(t *testing.T) {
	ref, err := DecodeProjectEvent([]byte(
		`{"object_id":"p1","object_type":"project","body":{"data":{"uid":"p1","slug":42,"is_foundation":"yes"}}}`))
	if err != nil {
		t.Fatalf("DecodeProjectEvent() = %v, want no error", err)
	}
	if ref.Slug != "" || ref.IsFoundation {
		t.Errorf("ref = %+v, want the surprising fields left at their zero values", ref)
	}
}
