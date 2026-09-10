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

// The subject this service publishes to, consumed by lfx-v2-indexer-service.
const (
	// IndexFormationSubject carries one checklist's search projection.
	//
	// The indexer subscribes to lfx.index.> and takes the object type from
	// whatever follows that prefix, so this constant alone decides that these
	// documents are searchable as type "formation". There is no registration
	// step and no allowlist on the indexer side to add this type to.
	//
	// Publish only. Nothing subscribes to this subject in this service, and
	// nothing here reads back what it published: the projection is derived from
	// Postgres, which stays the source of truth.
	IndexFormationSubject = "lfx.index.formation"
)

// The subjects this service consumes, published by lfx-v2-indexer-service after
// it has successfully written a project document.
//
// Named individually rather than matched with a wildcard. `lfx.project.>` would
// pick up whatever actions are added later and hand this service messages it has
// no branch for, and the one action that exists today and is deliberately absent
// below is the one a wildcard would quietly start delivering.
const (
	// ProjectCreatedSubject announces a project document that was just indexed
	// for the first time.
	ProjectCreatedSubject = "lfx.project.created"

	// ProjectUpdatedSubject announces a project document that was re-indexed,
	// which is where a stage change arrives.
	ProjectUpdatedSubject = "lfx.project.updated"

	// ProjectEventsQueue shares each message across this service's replicas so
	// exactly one of them handles it.
	//
	// Scoped to this service by name, which is what makes the sharing safe:
	// other services consuming the same subjects use their own queue and
	// receive their own copy. A name that collided with another service's would
	// take that service's messages instead of duplicating them, and nothing
	// about either subscription would look wrong from the inside.
	ProjectEventsQueue = "formation-service-project-events"
)

// Deliberately not consumed: lfx.project.deleted.
//
// A checklist is never deleted, so a project's disappearance cannot change one.
// What it can orphan is the queue row, and reacting to a delete event is the
// worst available way to handle that — the transport may lose the message, and
// unlike every other event here there is no sweep behind it to notice, because
// the sweep lists forming projects and a project that is gone appears in no
// list. A lost delete would orphan a row permanently. Removal is an
// operator-run repair instead; see IndexerPublisher.DeleteFormation.

// serviceAccountBearer is the Authorization header value used when a publish
// has no user behind it, which for this service is every publish: projections
// are built by the reconcile sweep, on a ticker, with no request context.
//
// Deliberately not a real token. The indexer refuses a V2 message whose
// authorization header is absent or empty, but its principal parser treats a
// non-JWT value as simply carrying no principal — it logs at debug and moves
// on. So this satisfies the envelope requirement while stating plainly that no
// person is behind the write, which is the truth: the alternative is minting an
// M2M token to attribute a sweep to a machine account nobody will look up.
//
// The value follows member-service's constant, itself following
// meeting-service's convention, so an operator grepping the indexer's logs for
// one of these finds all of them.
const serviceAccountBearer = "Bearer lfx-v2-formation-service"
