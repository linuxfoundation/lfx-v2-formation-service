// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// projectEvent is the subset of the indexer's published event this service
// reads.
//
// Declared here rather than imported, for the same reason indexerMessage is: the
// indexer's own type lives in its internal package and is not importable across
// the module boundary. The cost is that a change on that side does not break
// this build — it arrives as a field that stops being populated. Which is why
// the decode below treats every field as optional and says what it does when one
// is missing, instead of assuming a shape it cannot be compiled against.
type projectEvent struct {
	ObjectID   string `json:"object_id"`
	ObjectType string `json:"object_type"`
	Action     string `json:"action"`
	Body       *struct {
		ParentRefs []string       `json:"parent_refs"`
		Data       map[string]any `json:"data"`
	} `json:"body"`
}

// projectObjectType is the object type this service reacts to. Other types
// arrive on their own subjects, but the check is cheap and a wrong subscription
// is otherwise silent.
const projectObjectType = "project"

// DecodeProjectEvent turns a published project event into the same ProjectRef
// the sweep builds from the project list.
//
// Producing the identical type is the point. The listener's whole claim to
// correctness is that it hands the reconcile exactly what a sweep would have
// handed it, so the two paths cannot disagree about what a project is.
//
// Verified against the dev index rather than inferred from the publisher: the
// document body carries uid, slug, stage and parent_uid under data, and
// parent_refs alongside it in the prefixed "project:<uid>" form. is_foundation
// is omitted when false, which is why its absence is not an error.
//
// The stage is returned as-is, including empty. Deciding what an absent stage
// means is the listener's business and it is a decision with consequences —
// reading it as "not forming" would freeze live checklists — so this does not
// quietly substitute a default.
func DecodeProjectEvent(data []byte) (port.ProjectRef, error) {
	var event projectEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return port.ProjectRef{}, fmt.Errorf("decoding the project event: %w", err)
	}

	if event.ObjectType != "" && event.ObjectType != projectObjectType {
		return port.ProjectRef{}, fmt.Errorf("event is for object type %q, not a project", event.ObjectType)
	}
	if event.Body == nil {
		return port.ProjectRef{}, fmt.Errorf("event carries no body")
	}

	// The envelope's object_id is the same UID and is the more reliable of the
	// two: it is what the indexer keyed the document on, where data is the body
	// the publishing service supplied. Stated once, as a precedence, rather than
	// assigned from the body and then overwritten — which read as though the
	// body's value were being used.
	uid := event.ObjectID
	if uid == "" {
		uid = stringField(event.Body.Data, "uid")
	}
	if uid == "" {
		return port.ProjectRef{}, fmt.Errorf("event identifies no project")
	}

	ref := port.ProjectRef{
		UID:          uid,
		Slug:         stringField(event.Body.Data, "slug"),
		SubStage:     stringField(event.Body.Data, "stage"),
		ParentUID:    stringField(event.Body.Data, "parent_uid"),
		IsFoundation: boolField(event.Body.Data, "is_foundation"),
	}

	// parent_refs is the fallback rather than the primary: it is prefixed and
	// may name more than one ancestor, where data.parent_uid is the direct
	// parent stated plainly.
	if ref.ParentUID == "" {
		ref.ParentUID = firstParentUID(event.Body.ParentRefs)
	}

	return ref, nil
}

// firstParentUID strips the object-type prefix from the first parent reference.
func firstParentUID(refs []string) string {
	for _, ref := range refs {
		if uid, ok := strings.CutPrefix(ref, projectRefPrefix); ok && uid != "" {
			return uid
		}
	}
	return ""
}

// stringField reads a string from the document body, or empty if it is absent
// or is some other type.
func stringField(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
}

// boolField reads a bool from the document body, or false if it is absent.
//
// Absent is the common case rather than an anomaly: the publisher omits the
// field when it is false, so most documents carry no is_foundation at all.
func boolField(data map[string]any, key string) bool {
	value, _ := data[key].(bool)
	return value
}
