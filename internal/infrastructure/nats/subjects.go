// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

// Subjects served by lfx-v2-project-service. Every constant here was read from
// that service's own subscription list rather than assumed, because a subject
// this service invents is a request nothing answers — which surfaces as a
// request timeout rather than as anything naming the mistake.
//
// Each takes the project UID as the raw request body. Replies are raw bytes for
// the single-attribute lookups and JSON for the writers list.
const (
	// ProjectGetNameSubject resolves a project's display name.
	ProjectGetNameSubject = "lfx.projects-api.get_name"

	// ProjectGetSlugSubject resolves a project's slug. It is UID to slug; the
	// reverse lookup is a different subject.
	ProjectGetSlugSubject = "lfx.projects-api.get_slug"

	// ProjectGetWritersSubject returns the project's writers as a JSON array,
	// empty when none are configured.
	//
	// Superseded for assignment validation by ProjectGetSettingsSubject, which
	// answers with the auditors as well. Kept because it is the narrower read:
	// a caller that only needs writers should not receive the auditors and the
	// announcement date alongside them.
	ProjectGetWritersSubject = "lfx.projects-api.get_writers"

	// ProjectGetSettingsSubject returns the project's grant roster and its
	// announcement date from the one settings record that holds all three.
	//
	// Takes the project UID as the raw body and replies with JSON. This is the
	// read assignment validation needs: writers alone would refuse every
	// legitimate auditor, so a partial answer here is worse than none.
	ProjectGetSettingsSubject = "lfx.projects-api.get_settings"

	// ProjectListProjectsSubject returns projects by stage, by UID, or the
	// union of both.
	//
	// The only subject here whose request body is JSON rather than a bare UID,
	// and the only one that answers a caller holding no UID at all. The union
	// is what the reconcile sweep needs in a single round trip: the stages it
	// watches, plus the projects it already holds a checklist for, which may
	// have left those stages and still need their lifecycle moved.
	ProjectListProjectsSubject = "lfx.projects-api.list_projects"
)
