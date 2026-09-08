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
	// This is the only role the project service exposes. There is no auditors
	// equivalent and no subject that returns the settings record whole, which
	// is why this package cannot yet answer the writers-union-auditors question
	// assignment validation asks. See project_client.go.
	ProjectGetWritersSubject = "lfx.projects-api.get_writers"
)
