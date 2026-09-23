// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

func sampleAccess() port.ApplicationAccess {
	return port.ApplicationAccess{
		ApplicationUID:    "8b1f6f2e-0000-4000-8000-000000000001",
		SubmitterUsername: "jdoe",
		FormationTeam:     "formation",
	}
}

// The grant names the submitter and the formation team, and nobody wider.
//
// The team travels as a reference rather than a relation because its grantee
// is a userset: fga-sync prefixes relation values with `user:`, which would
// turn the team into a person who does not exist, and passes reference values
// containing a colon through unchanged.
func TestPublishApplicationAccessGrantsTheSubmitterAndTheTeam(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, FGAUpdateAccessSubject)
	publisher := NewAccessPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.PublishApplicationAccess(context.Background(), sampleAccess()); err != nil {
		t.Fatalf("PublishApplicationAccess() = %v, want no error", err)
	}

	raw := await()

	var message struct {
		ObjectType string `json:"object_type"`
		Operation  string `json:"operation"`
		Data       struct {
			UID        string              `json:"uid"`
			Public     bool                `json:"public"`
			Relations  map[string][]string `json:"relations"`
			References map[string][]string `json:"references"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatalf("Unmarshal() = %v, want no error\npayload: %s", err, raw)
	}

	if message.ObjectType != "project_application" {
		t.Errorf("object_type = %q, want project_application", message.ObjectType)
	}
	if message.Data.UID != sampleAccess().ApplicationUID {
		t.Errorf("uid = %q, want the application uid", message.Data.UID)
	}
	if message.Data.Public {
		t.Error("public = true, which writes user:* as viewer on a record carrying a named " +
			"person's email")
	}

	if got := message.Data.Relations["submitter"]; len(got) != 1 || got[0] != "jdoe" {
		t.Errorf("relations[submitter] = %v, want [jdoe]", got)
	}
	if got := message.Data.References["formation_team"]; len(got) != 1 || got[0] != "team:formation#member" {
		t.Errorf("references[formation_team] = %v, want [team:formation#member]", got)
	}
	if len(message.Data.Relations) != 1 || len(message.Data.References) != 1 {
		t.Errorf("the grant names more than the submitter and the team: relations=%v references=%v",
			message.Data.Relations, message.Data.References)
	}
	if strings.Contains(string(raw), "viewer") {
		t.Errorf("the grant writes derived relation \"viewer\" directly\npayload: %s", raw)
	}
}

// Every field is required, and an incomplete grant is refused rather than
// published.
//
// This matters more than the usual argument check because the update is a
// full sync: fga-sync removes any relation the message does not carry. A grant
// missing the submitter therefore does not merely fail to add them — it
// deletes the tuple they already had, locking the applicant out of their own
// application with no error anywhere, since the publish is fire-and-forget.
func TestPublishApplicationAccessRefusesAnIncompleteGrant(t *testing.T) {
	url := startTestNATSServer(t)
	publisher := NewAccessPublisher(newTestClient(t, url, 2*time.Second))

	cases := map[string]func(a *port.ApplicationAccess){
		"no uid":       func(a *port.ApplicationAccess) { a.ApplicationUID = "" },
		"no submitter": func(a *port.ApplicationAccess) { a.SubmitterUsername = "" },
		"no team":      func(a *port.ApplicationAccess) { a.FormationTeam = "" },
	}

	for name, blank := range cases {
		t.Run(name, func(t *testing.T) {
			access := sampleAccess()
			blank(&access)
			if err := publisher.PublishApplicationAccess(context.Background(), access); err == nil {
				t.Error("PublishApplicationAccess() = nil, want an error — a full sync missing a " +
					"relation revokes it")
			}
		})
	}
}

// The revocation travels on the delete subject, not the update subject.
//
// An empty Relations map on the update subject would look like a revocation
// and is not one: the far side treats a full sync with no relations as a sync
// with no relations, and whether that removes anything is its decision, not
// ours. The delete operation says what is meant.
func TestDeleteApplicationAccessTravelsOnTheDeleteSubject(t *testing.T) {
	url := startTestNATSServer(t)
	await := captureOn(t, url, FGADeleteAccessSubject)
	publisher := NewAccessPublisher(newTestClient(t, url, 2*time.Second))

	uid := sampleAccess().ApplicationUID
	if err := publisher.DeleteApplicationAccess(context.Background(), uid); err != nil {
		t.Fatalf("DeleteApplicationAccess() = %v, want no error", err)
	}

	var message struct {
		ObjectType string `json:"object_type"`
		Operation  string `json:"operation"`
		Data       struct {
			UID string `json:"uid"`
		} `json:"data"`
	}
	raw := await()
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatalf("Unmarshal() = %v, want no error\npayload: %s", err, raw)
	}

	if message.ObjectType != "project_application" {
		t.Errorf("object_type = %q, want project_application", message.ObjectType)
	}
	if message.Operation != "delete_access" {
		t.Errorf("operation = %q, want delete_access", message.Operation)
	}
	if message.Data.UID != uid {
		t.Errorf("uid = %q, want %q", message.Data.UID, uid)
	}
}

func TestDeleteApplicationAccessRefusesAnEmptyUID(t *testing.T) {
	url := startTestNATSServer(t)
	publisher := NewAccessPublisher(newTestClient(t, url, 2*time.Second))

	if err := publisher.DeleteApplicationAccess(context.Background(), ""); err == nil {
		t.Error("DeleteApplicationAccess(\"\") = nil, want an error")
	}
}
