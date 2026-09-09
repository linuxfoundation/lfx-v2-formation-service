// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// The authorization test the queue is required to have, in its own file rather
// than among the projection's other tests.
//
// The projection is the only place in this service where data leaves the request
// path and lands somewhere another service decides who may read it. Every other
// read is gated at the gateway on the route being called; this one is gated on a
// string inside a message. A widening here — the relation changed to viewer, an
// assignee's name reaching a field meant to hold titles — is invisible to every
// other test in the package and would ship as a silent disclosure rather than as
// a failure.
//
// These assert the value, not merely that a value is present: "a relation is
// set" passes just as happily on viewer.

func TestProjectionIsGatedOnAuditorAndNeverOnViewer(t *testing.T) {
	doc := buildProjection(
		&model.Formation{ProjectUID: "project-1", Lifecycle: model.LifecycleLive},
		nil, port.ProjectRef{}, "A Project", "",
	)

	if doc.AccessRelation == "viewer" {
		t.Fatal("access relation = viewer — the FGA model grants project#viewer to user:*, " +
			"so this would publish a confidential project's checklist to every authenticated user")
	}
	if doc.AccessRelation != "auditor" {
		t.Errorf("access relation = %q, want auditor", doc.AccessRelation)
	}
}

// The constant is asserted directly as well as through the builder. The builder
// could stop setting it — that is what the test above catches — but the constant
// could also be edited in place, which would leave the builder passing while
// changing what it means.
func TestTheAccessRelationConstantIsAuditor(t *testing.T) {
	if formationAccessRelation != "auditor" {
		t.Errorf("formationAccessRelation = %q, want auditor. Read the constant's comment "+
			"before changing this: viewer is public.", formationAccessRelation)
	}
}

// The Blocking column is titles only. An assignee reaching it would put who is
// stuck on what into a document read by everyone holding the project's audit
// relation, which is a wider audience than the people working the checklist.
func TestBlockedTitlesNameNoPerson(t *testing.T) {
	doc := buildProjection(
		&model.Formation{ProjectUID: "project-1", Lifecycle: model.LifecycleLive},
		[]*model.Item{
			{ItemKey: "charter", Title: "Charter agreed", Status: model.StatusBlocked, Assignee: "person-one"},
			{ItemKey: "brand", Title: "Brand review", Status: model.StatusInProgress, Assignee: "person-two"},
		},
		port.ProjectRef{}, "A Project", "",
	)

	for _, title := range doc.BlockedItemTitles {
		for _, person := range []string{"person-one", "person-two"} {
			if strings.Contains(title, person) {
				t.Errorf("blocked title %q names a person", title)
			}
		}
	}
	if len(doc.BlockedItemTitles) != 1 || doc.BlockedItemTitles[0] != "Charter agreed" {
		t.Errorf("blocked titles = %v, want only the blocked item's title", doc.BlockedItemTitles)
	}
}

// Notes and skip reasons are free text a writer typed, often about a partner or a
// negotiation. The projection has no field for either, and this asserts that
// nothing on it can carry one — so adding such a field is a decision rather than
// an oversight.
func TestProjectionCarriesNoFreeTextFromItems(t *testing.T) {
	const secret = "confidential-note-do-not-index"
	doc := buildProjection(
		&model.Formation{ProjectUID: "project-1", Lifecycle: model.LifecycleLive},
		[]*model.Item{{
			ItemKey: "charter", Title: "Charter agreed", Status: model.StatusBlocked,
			Note: secret, SkipReason: secret, EvidenceLink: "https://example.org/" + secret,
		}},
		port.ProjectRef{}, "A Project", "",
	)

	for _, title := range doc.BlockedItemTitles {
		if strings.Contains(title, secret) {
			t.Errorf("a note reached blocked_item_titles: %q", title)
		}
	}
	for _, assignee := range doc.Assignees {
		if strings.Contains(assignee, secret) {
			t.Errorf("a note reached assignees: %q", assignee)
		}
	}
}
