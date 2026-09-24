// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

// Application relation names used for tuple writes and indexed access checks.
//
// Spelled once because relation strings are not validated against the
// authorization model before use: a misspelling never matches a check and
// surfaces only as "this person has no access."
//
// Prefixed with the type they belong to, unlike committee-service's bare
// RelationWriter and RelationAuditor. Necessary here rather than a stylistic
// choice: `model.RelationFormationTeam` already exists and holds
// "formation_team_member", which is the advisory label a checklist item
// publishes to tell a browser what an action requires. That is a different
// string for a different purpose, and two constants of the same short name
// would be swappable by mistake in exactly the place where the mistake is
// silent.
const (
	// RelationApplicationSubmitter is the applicant's grant on their own
	// application. Both viewer and writer resolve through it in the platform
	// model, so reading, revising, withdrawing and deleting all come from
	// this one tuple.
	RelationApplicationSubmitter = "submitter"

	// RelationApplicationFormationTeam is the staff grant on an application,
	// and is checked directly for accepting and denying — which is what keeps
	// those two out of the submitter's reach, since nothing grants it to them.
	RelationApplicationFormationTeam = "formation_team"

	// RelationApplicationViewer is what a caller must hold to read an
	// application's indexed document. It is not the public wildcard the name
	// carries elsewhere on this model; project_application defines it the way
	// committee_invite does, resolving through the subject relation only.
	RelationApplicationViewer = "viewer"
)
