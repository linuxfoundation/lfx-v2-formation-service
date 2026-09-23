// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package design defines the Goa API and service endpoints.
// Run `make apigen` to regenerate the code in gen/ after modifying this file.
package design

import "goa.design/goa/v3/dsl"

var _ = dsl.API("lfx-v2-formation-service", func() {
	dsl.Title("LFX V2 Formation Service")
	dsl.Version("1.0")
})

// JWTAuth is the DSL JWT security type for authentication.
var JWTAuth = dsl.JWTSecurity("jwt", func() {
	dsl.Description("Heimdall authorization")
})

// BearerTokenAttribute is the DSL attribute for bearer token.
func BearerTokenAttribute() {
	dsl.Token("bearer_token", dsl.String, func() {
		dsl.Description("JWT token issued by Heimdall")
		dsl.Example("eyJhbGci...")
	})
}

// VersionAttribute is the required API-version attribute every route carries,
// mapped to the ?v= query parameter. Mirrors lfx-v2-committee-service's
// cmd/committee-api/design/type.go: a client pins the contract it was written
// against, so a future breaking response shape can ship as v=2 while v=1
// callers keep the old one. Omitting it is a 400, and the LFX One BFF already
// sends v=1 on every proxied request (its DEFAULT_QUERY_PARAMS), so this is
// satisfied by the platform's existing client without any UI change.
func VersionAttribute() {
	dsl.Attribute("version", dsl.String, "API version. Must be 1.", func() {
		dsl.Enum("1")
		dsl.Example("1")
	})
}

// ETagAttribute carries an item's new version back in the response, so a
// caller holding the result already holds the If-Match for its next write.
//
// Bare digits, not a quoted entity-tag: if_match is an Int64, so a caller
// echoing a quoted value straight back would be refused at decode time.
// Matching what the platform's other Goa services emit.
func ETagAttribute() {
	dsl.Attribute("etag", dsl.String, "The item's new version. Send as If-Match on the next write.", func() {
		dsl.Example("8")
	})
}

func ApplicationETagAttribute() {
	dsl.Attribute("etag", dsl.String, "The application's new revision. Send as If-Match on the next write.", func() {
		dsl.Example("2")
	})
}

var _ = dsl.Service("lfx_v2_formation_service", func() {
	dsl.Description("LFX V2 Formation Service")

	dsl.Method("get_formation", func() {
		dsl.Description("Return the whole checklist for a project in one response — sections, items, progress and readiness. Items are never fetched individually.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("project_uid", dsl.String, "The project's UID.")
			dsl.Required("version", "project_uid")
		})
		dsl.Result(FormationChecklist)
		dsl.Error("NotFound", NotFoundError, "The requested resource does not exist")
		dsl.Error("Unauthorized", UnauthorizedError, "Missing, expired, or malformed bearer token")
		dsl.HTTP(func() {
			dsl.GET("/formations/{project_uid}")
			dsl.Param("version:v")
			dsl.Header("bearer_token:Authorization")
			dsl.Response(dsl.StatusOK)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("Unauthorized", dsl.StatusUnauthorized)
		})
	})

	dsl.Method("get_formation_activity", func() {
		dsl.Description("Return the formation's activity feed, newest first, with ULID cursor paging. " +
			"The feed covers checklist changes only — status changes, assignment, notes, links, skip " +
			"reasons and template work. Permission changes never appear here: nothing keeps a history of " +
			"them, since each save overwrites the previous state. " +
			"Pass item_uid to narrow the feed to one item's history. A cursor belongs to the sequence " +
			"that produced it, not to the feed generally: a next_cursor from a filtered read is only " +
			"valid when replayed with the same item_uid, and one from an unfiltered read only without " +
			"one. Mixing them is not rejected and does not error — it returns a correct page of a " +
			"different sequence, which is the dangerous outcome, so a caller must carry the filter " +
			"alongside the cursor.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("project_uid", dsl.String, "The project's UID.")
			dsl.Attribute("cursor", dsl.String, "Opaque ULID cursor from a previous page's next_cursor. Omit for the first page.")
			// The item's UID, not its stable item_key, which is what every
			// route that mutates an item takes. Deliberate and stated here
			// rather than left to be inferred: an activity row stores the
			// item's UID as a foreign key and does not store the key at all,
			// so a consumer filtering the feed already holds the UID from the
			// entries it is reading, and requiring the key would mean
			// resolving an identity the row already carries. A consumer that
			// guesses wrong gets a silent empty feed rather than an error,
			// which is why the identifier is named in the description.
			//
			// Format is what makes a malformed value a decode-time 400 rather
			// than reaching a UUID column and surfacing as a 500 — the same
			// reasoning as the limit minimum below.
			dsl.Attribute("item_uid", dsl.String,
				"Narrow the feed to one checklist item's history. This is the item's UID (as carried on "+
					"each entry's item_uid), not the stable item_key the mutation routes take. Omit for "+
					"the whole checklist's feed.",
				func() { dsl.Format(dsl.FormatUUID) })
			dsl.Attribute("limit", dsl.Int, "Page size. Defaults to 20, capped at 100.", func() {
				dsl.Default(20)
				// Without a minimum, limit=-1 passes decode and the service
				// quietly coerces it to the default, answering a different
				// request than the one asked.
				dsl.Minimum(1)
				dsl.Maximum(100)
			})
			dsl.Required("version", "project_uid")
		})
		dsl.Result(FormationActivityPage)
		// One error type for both not-found cases, told apart by the
		// response body's message field alone, at runtime — never by this
		// description, which Goa renders once into the OpenAPI document for
		// every method sharing NotFoundError. A per-method description here
		// would make one method's text win for all of them (as it did before
		// this was aligned to get_formation's), silently misdocumenting
		// whichever method didn't win. Goa also needs an attribute tagged
		// Meta("struct:error:name") to disambiguate two custom errors on one
		// method, and NotFoundError carries only code and message — so
		// adding a second error, or a discriminator field, would change the
		// 404 body for the existing formation case, which a caller that
		// sends no item_uid must not see. The two messages are: "no
		// formation exists for this project" and "no such item in this
		// formation". The item case is deliberately uniform — an item in a
		// project the caller cannot see reads exactly like an item that
		// exists nowhere, so this route is not an existence oracle for other
		// projects' items.
		dsl.Error("NotFound", NotFoundError, "The requested resource does not exist")
		dsl.Error("Unauthorized", UnauthorizedError, "Missing, expired, or malformed bearer token")
		dsl.HTTP(func() {
			dsl.GET("/formations/{project_uid}/activity")
			dsl.Param("version:v")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("cursor")
			dsl.Param("item_uid")
			dsl.Param("limit")
			dsl.Response(dsl.StatusOK)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("Unauthorized", dsl.StatusUnauthorized)
		})
	})

	// Three routes touch one item, and the split is the guard rather than a
	// taste for small payloads. The architecture review gives three tiers:
	// leaving an update takes read access, directing somebody else's work takes
	// write access, and judging whether work was done takes write access plus
	// membership of the formation team.
	//
	// The gateway could carry all three on one route — it can read the parsed
	// body and run an authorizer conditionally on what it finds. The tiers are
	// split by route because the review's API surface asks for that, and
	// because a conditional guard fails open: should the condition ever stop
	// matching the payload it is meant to catch, the team check quietly does
	// not run, and what it prevents is somebody closing their own item. A
	// field's tier therefore decides its route, and folding any two together
	// would widen the stricter guard to the broadest field sharing the route.

	// Tier three, the narrowest: anything that moves a status.
	//
	// This guard is what keeps an assignee from closing their own item. A
	// partner elevated to writer in order to create the committee still is not
	// on the formation team, so they do the work, leave a note on the PATCH
	// route, and somebody else judges it.
	//
	// Sub-items are here rather than with the fields because a sub-item status
	// is a status, drawn from the same enum, and the review treats every status
	// write alike. Putting them on a wider route would let somebody march an
	// item's sub-items to done without holding what closing the item takes.
	dsl.Method("set_item_status", func() {
		dsl.Description("Move one checklist item to a new status — in progress, blocked, done, " +
			"skipped, or back to not started — or set the status of its sub-items. At least one of " +
			"status and sub_items is required; both may travel together. If-Match is required and " +
			"must equal the item's current version — a stale value means re-read and retry. The " +
			"response returns the new version as ETag. Blocking, skipping and sending an item back " +
			"each require a reason; the other transitions ignore one if sent.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("project_uid", dsl.String, "The project's UID.")
			dsl.Attribute("item_key", dsl.String, "The item's stable key.")
			dsl.Attribute("if_match", dsl.Int64, "Must equal the item's current version.")
			// Optional, so a caller may move a sub-item without restating
			// the parent's status — restating it would be refused as a
			// no-op, which would make sub-items unreachable.
			dsl.Attribute("status", dsl.String, "One of the five. Omit to change sub-items alone.", func() {
				dsl.Enum("not_started", "in_progress", "blocked", "done", "skipped")
			})
			// One field for all three, rather than a skip reason and a
			// blocking note carrying the same sentence under different
			// names. Where it comes to rest still differs — skipping
			// records it as the item's skip reason, the others as its
			// note — because those are read back in different places.
			dsl.Attribute("reason", dsl.String,
				"Why. Required when blocking, skipping, or sending an item back to not started: each "+
					"leaves somebody with work to redo, and a bare status change tells them nothing.")
			dsl.Attribute("sub_items", dsl.ArrayOf(FormationSubItemUpdate))
			dsl.Required("version", "project_uid", "item_key", "if_match")
		})
		dsl.Result(func() {
			dsl.Attribute("item", FormationItem)
			ETagAttribute()
			dsl.Required("item")
		})
		dsl.Error("NotFound", FormationError, "No formation, or no item with that key, exists")
		dsl.Error("VersionMismatch", FormationError, "If-Match did not match the item's current version")
		dsl.Error("Conflict", FormationError, "The checklist, or this item's current state, refuses the change")
		dsl.Error("BadRequest", FormationError, "The payload itself is invalid")
		dsl.Error("Unauthorized", UnauthorizedError, "Missing, expired, or malformed bearer token")
		dsl.HTTP(func() {
			dsl.POST("/formations/{project_uid}/items/{item_key}/status")
			dsl.Param("version:v")
			dsl.Header("bearer_token:Authorization")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("item")
				dsl.Header("etag:ETag")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("VersionMismatch", dsl.StatusPreconditionFailed)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Unauthorized", dsl.StatusUnauthorized)
		})
	})

	// Tier two: directing somebody else's work, on write access.
	//
	// Assignment and the due date share a route because they are the same act —
	// telling a named person what to do and by when — and the review puts them
	// on the same guard. Neither judges whether anything was done, which is why
	// they do not need the team check, and neither is a mere record of work, so
	// read access is not enough.
	dsl.Method("assign_item", func() {
		dsl.Description("Direct one checklist item's work: set or clear its assignee, set or clear " +
			"its due date. Send only the fields being changed; at least one is required. Assignment " +
			"is limited to people already holding a grant on the project. If-Match is required and " +
			"must equal the item's current version. The response returns the new version as ETag.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("project_uid", dsl.String, "The project's UID.")
			dsl.Attribute("item_key", dsl.String, "The item's stable key.")
			dsl.Attribute("if_match", dsl.Int64, "Must equal the item's current version.")
			dsl.Attribute("assignee", dsl.String, "Username, or an empty string to clear.")
			// No dsl.Format(FormatDate) here, unlike the read-side
			// due_date attribute: FormatDate is enforced at request
			// decode time, before this string ever reaches the service,
			// which would make an empty string (this field's clear
			// signal) always fail validation and give a caller no way to
			// clear a due date at all. The service validates the format
			// itself for a non-empty value instead.
			// Examples are set by hand on both: the format of each is enforced
			// in the service rather than the DSL, so Goa would otherwise
			// invent a random sentence and the published documents would
			// advertise a value that earns a 400.
			dsl.Attribute("due_date", dsl.String, "YYYY-MM-DD, or an empty string to clear.", func() {
				dsl.Example("2026-03-31")
			})
			dsl.Required("version", "project_uid", "item_key", "if_match")
		})
		dsl.Result(func() {
			dsl.Attribute("item", FormationItem)
			ETagAttribute()
			dsl.Required("item")
		})
		dsl.Error("NotFound", FormationError, "No formation, or no item with that key, exists")
		dsl.Error("VersionMismatch", FormationError, "If-Match did not match the item's current version")
		dsl.Error("Conflict", FormationError, "The checklist, or this item's current state, refuses the change")
		dsl.Error("BadRequest", FormationError, "The payload itself is invalid")
		dsl.Error("Unauthorized", UnauthorizedError, "Missing, expired, or malformed bearer token")
		dsl.HTTP(func() {
			dsl.POST("/formations/{project_uid}/items/{item_key}/assignment")
			dsl.Param("version:v")
			dsl.Header("bearer_token:Authorization")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("item")
				dsl.Header("etag:ETag")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("VersionMismatch", dsl.StatusPreconditionFailed)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Unauthorized", dsl.StatusUnauthorized)
		})
	})

	// Tier one, the widest: leaving an update, on read access alone.
	//
	// Read access and not write access, and that is the point of the route
	// rather than an oversight. The assignee of an item is often a partner
	// holding only "View": they do the work off-platform — a trademark search,
	// a DocuSign, a domain transfer — and record here what they did, and
	// somebody on the formation team reads it and moves the status. Guarding
	// this on write access would mean the person doing the work cannot say they
	// did it, which is the whole partner workflow.
	dsl.Method("update_item", func() {
		dsl.Description("Leave an update on one checklist item: a note, an evidence link. Neither " +
			"moves a status nor directs anybody's work, so this is the one item route open on read " +
			"access. Send only the fields being changed; at least one is required. If-Match is " +
			"required and must equal the item's current version — a stale value means re-read and " +
			"retry. The response returns the new version as ETag.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("project_uid", dsl.String, "The project's UID.")
			dsl.Attribute("item_key", dsl.String, "The item's stable key.")
			dsl.Attribute("if_match", dsl.Int64, "Must equal the item's current version.")
			dsl.Attribute("note", dsl.String)
			dsl.Attribute("evidence_link", dsl.String, "Feeds Quick Links. http/https only.", func() {
				dsl.Example("https://example.org/bylaws.pdf")
			})
			dsl.Required("version", "project_uid", "item_key", "if_match")
		})
		dsl.Result(func() {
			dsl.Attribute("item", FormationItem)
			ETagAttribute()
			dsl.Required("item")
		})
		dsl.Error("NotFound", FormationError, "No formation, or no item with that key, exists")
		dsl.Error("VersionMismatch", FormationError, "If-Match did not match the item's current version")
		dsl.Error("Conflict", FormationError, "The checklist, or this item's current state, refuses the change")
		dsl.Error("BadRequest", FormationError, "The payload itself is invalid")
		dsl.Error("Unauthorized", UnauthorizedError, "Missing, expired, or malformed bearer token")
		dsl.HTTP(func() {
			dsl.PATCH("/formations/{project_uid}/items/{item_key}")
			dsl.Param("version:v")
			dsl.Header("bearer_token:Authorization")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("item")
				dsl.Header("etag:ETag")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("VersionMismatch", dsl.StatusPreconditionFailed)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Unauthorized", dsl.StatusUnauthorized)
		})
	})

	// Project applications. These are the first routes here that are not
	// under /formations/{project_uid}, and that is the object model rather
	// than a naming choice: an application exists before any project does, so
	// there is no project UID to key it by and no project to resolve a
	// permission through.
	//
	// There is deliberately no collection route and no /me-scoped route.
	// Listing — both a submitter's own applications and the staff review queue
	// — is served by the query service over the indexed document, which
	// resolves the reader's access per document. A collection endpoint here
	// would have to re-derive that access in Go from a stored attribute, which
	// is the design the architecture review names as the alternative it does
	// not recommend, and it is how the two audiences' rules drift apart.
	//
	// There is no read-one route either, for the same reason: the query
	// service already serves it, and a second read path is a second place for
	// the access rule to be decided.
	dsl.Method("create_application", func() {
		dsl.Description("Submit an application to start a new foundation. Creates an application " +
			"record and nothing else — no project, no checklist, no stage, no entity placement. " +
			"The submitter's identity is recorded from the payload rather than from the caller's " +
			"token: this route is called by the UI authenticating as itself, so the end user never " +
			"presents a credential here. Anti-automation belongs to that caller; this service adds " +
			"no second control.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()

			// Recorded as data, and named so on the wire. A caller holding the
			// intake grant can name any submitter, which is a property of the
			// create guard rather than something this route can check.
			dsl.Attribute("submitter_username", dsl.String, "The applicant's username, as the calling UI knows it.")
			dsl.Attribute("submitter_name", dsl.String, "The applicant's display name.")
			dsl.Attribute("submitter_email", dsl.String, "The applicant's email address.", func() {
				dsl.Format(dsl.FormatEmail)
			})

			// A hint, never a placement. The incorporated entity is chosen by
			// whoever approves, and is deliberately not asked at intake, so
			// nothing downstream may read this as where the project will sit.
			dsl.Attribute("target_parent_uid", dsl.String,
				"Optional. Where the applicant started from, carried as a hint to prefill the "+
					"approver's form. It does not decide the parent or the incorporated entity, and "+
					"it grants nobody anything. Normally absent.")

			// The source names the intake fields but does not define their wire
			// keys, types or requiredness. Keep the questionnaire as one map
			// rather than inventing a typed contract.
			dsl.Attribute("application", dsl.MapOf(dsl.String, dsl.Any),
				"The intake answers. Carries the proposed project's website as a URL. People "+
					"named for the formation work are email addresses only — they are not "+
					"resolved to platform identities, granted anything, or notified.")

			dsl.Required("version", "submitter_username", "submitter_name", "submitter_email", "application")
		})
		dsl.Result(ProjectApplication)
		dsl.Error("BadRequest", ApplicationError, "The payload itself is invalid")
		dsl.Error("Unauthorized", UnauthorizedError, "Missing, expired, or malformed bearer token")
		dsl.HTTP(func() {
			dsl.POST("/project-applications")
			dsl.Param("version:v")
			dsl.Header("bearer_token:Authorization")
			dsl.Response(dsl.StatusCreated)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Unauthorized", dsl.StatusUnauthorized)
		})
	})

	// Revise and withdraw are one route each, open to the submitter and to
	// the formation team alike. Neither carries an audience in its path or in
	// its payload: the gateway resolves `writer` on the application object,
	// and the model grants that relation to both. A `/me`-scoped twin of
	// either route would be a second place for the same rule to be decided,
	// and the two copies drift.
	dsl.Method("revise_application", func() {
		dsl.Description("Replace an application's answers without changing its state.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "The application's unique identifier.", func() {
				dsl.Format(dsl.FormatUUID)
			})
			dsl.Attribute("if_match", dsl.Int64, "Must equal the application's current revision.")

			// A replacement rather than a merge. The questionnaire is an open
			// map, so a merge would have no way to express "remove this
			// answer" — a key absent from the request and a key the caller
			// meant to clear are the same bytes.
			dsl.Attribute("application", dsl.MapOf(dsl.String, dsl.Any),
				"The complete set of intake answers, replacing what is stored. Validated the same "+
					"way the original submission was.")

			dsl.Required("version", "uid", "if_match", "application")
		})
		dsl.Result(ProjectApplicationMutationResult)
		dsl.Error("BadRequest", ApplicationError, "The payload itself is invalid")
		dsl.Error("NotFound", ApplicationError, "No such application")
		dsl.Error("VersionMismatch", ApplicationError, "If-Match did not match the application's current revision")
		dsl.Error("Unauthorized", UnauthorizedError, "Missing, expired, or malformed bearer token")
		dsl.HTTP(func() {
			dsl.PUT("/project-applications/{uid}")
			dsl.Param("version:v")
			dsl.Header("bearer_token:Authorization")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("application")
				dsl.Header("etag:ETag")
			})
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("VersionMismatch", dsl.StatusPreconditionFailed)
			dsl.Response("Unauthorized", dsl.StatusUnauthorized)
		})
	})

	dsl.Method("withdraw_application", func() {
		dsl.Description("Withdraw an application and retain its record.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "The application's unique identifier.", func() {
				dsl.Format(dsl.FormatUUID)
			})
			dsl.Attribute("if_match", dsl.Int64, "Must equal the application's current revision.")

			dsl.Required("version", "uid", "if_match")
		})
		dsl.Result(ProjectApplicationMutationResult)
		dsl.Error("NotFound", ApplicationError, "No such application")
		dsl.Error("VersionMismatch", ApplicationError, "If-Match did not match the application's current revision")
		dsl.Error("Unauthorized", UnauthorizedError, "Missing, expired, or malformed bearer token")
		dsl.HTTP(func() {
			dsl.POST("/project-applications/{uid}/withdraw")
			dsl.Param("version:v")
			dsl.Header("bearer_token:Authorization")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("application")
				dsl.Header("etag:ETag")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("VersionMismatch", dsl.StatusPreconditionFailed)
			dsl.Response("Unauthorized", dsl.StatusUnauthorized)
		})
	})

	// Accepting and denying are separate routes rather than one route with a
	// decision in the payload.
	//
	// The gateway authorizes a route, not a field, so a single route would
	// have to admit anyone allowed to make either decision and then let the
	// service sort out which was asked for — putting half the authorization
	// question inside Go code, where these rules deliberately do not live.
	// Two routes means each one's guard is the whole of its guard.
	//
	// Both are guarded on `formation_team`, which the submitter does not
	// hold. That is what stops a submitter deciding their own application:
	// revising, withdrawing and deleting need `writer`, which they do hold,
	// and deciding needs a relation they do not.
	dsl.Method("accept_application", func() {
		dsl.Description("Accept an application without creating a project.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "The application's unique identifier.", func() {
				dsl.Format(dsl.FormatUUID)
			})
			dsl.Attribute("if_match", dsl.Int64, "Must equal the application's current revision.")
			dsl.Required("version", "uid", "if_match")
		})
		dsl.Result(ProjectApplicationMutationResult)
		dsl.Error("NotFound", ApplicationError, "No such application")
		dsl.Error("VersionMismatch", ApplicationError, "If-Match did not match the application's current revision")
		dsl.Error("Unauthorized", UnauthorizedError, "Missing, expired, or malformed bearer token")
		dsl.HTTP(func() {
			dsl.POST("/project-applications/{uid}/accept")
			dsl.Param("version:v")
			dsl.Header("bearer_token:Authorization")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("application")
				dsl.Header("etag:ETag")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("VersionMismatch", dsl.StatusPreconditionFailed)
			dsl.Response("Unauthorized", dsl.StatusUnauthorized)
		})
	})

	dsl.Method("deny_application", func() {
		dsl.Description("Deny an application and retain its record.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "The application's unique identifier.", func() {
				dsl.Format(dsl.FormatUUID)
			})
			dsl.Attribute("if_match", dsl.Int64, "Must equal the application's current revision.")
			dsl.Required("version", "uid", "if_match")
		})
		dsl.Result(ProjectApplicationMutationResult)
		dsl.Error("NotFound", ApplicationError, "No such application")
		dsl.Error("VersionMismatch", ApplicationError, "If-Match did not match the application's current revision")
		dsl.Error("Unauthorized", UnauthorizedError, "Missing, expired, or malformed bearer token")
		dsl.HTTP(func() {
			dsl.POST("/project-applications/{uid}/deny")
			dsl.Param("version:v")
			dsl.Header("bearer_token:Authorization")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("application")
				dsl.Header("etag:ETag")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("VersionMismatch", dsl.StatusPreconditionFailed)
			dsl.Response("Unauthorized", dsl.StatusUnauthorized)
		})
	})

	dsl.Method("delete_application", func() {
		dsl.Description("Delete an application from storage and search, and remove its submitter " +
			"grant. The formation-team tuple is retained.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "The application's unique identifier.", func() {
				dsl.Format(dsl.FormatUUID)
			})
			dsl.Attribute("if_match", dsl.Int64, "Must equal the application's current revision.")
			dsl.Required("version", "uid", "if_match")
		})
		dsl.Error("NotFound", ApplicationError, "No such application")
		dsl.Error("VersionMismatch", ApplicationError, "If-Match did not match the application's current revision")
		dsl.Error("Unauthorized", UnauthorizedError, "Missing, expired, or malformed bearer token")
		dsl.HTTP(func() {
			dsl.DELETE("/project-applications/{uid}")
			dsl.Param("version:v")
			dsl.Header("bearer_token:Authorization")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusNoContent)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("VersionMismatch", dsl.StatusPreconditionFailed)
			dsl.Response("Unauthorized", dsl.StatusUnauthorized)
		})
	})

	dsl.Method("livez", func() {
		dsl.Description("Liveness probe.")
		dsl.Meta("swagger:generate", "false")
		dsl.Result(dsl.Bytes, func() { dsl.Example("OK") })
		dsl.HTTP(func() {
			dsl.GET("/livez")
			dsl.Response(dsl.StatusOK, func() { dsl.ContentType("text/plain") })
		})
	})

	dsl.Method("readyz", func() {
		dsl.Description("Readiness probe.")
		dsl.Meta("swagger:generate", "false")
		dsl.Result(dsl.Bytes, func() { dsl.Example("OK") })
		dsl.Error("ServiceUnavailable", ServiceUnavailableError, "Service unavailable")
		dsl.HTTP(func() {
			dsl.GET("/readyz")
			dsl.Response(dsl.StatusOK, func() { dsl.ContentType("text/plain") })
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})
})

// ServiceUnavailableError is the DSL type for a service unavailable error.
var ServiceUnavailableError = dsl.Type("ServiceUnavailableError", func() {
	dsl.Attribute("code", dsl.String, "HTTP status code", func() { dsl.Example("503") })
	dsl.Attribute("message", dsl.String, "Error message", func() { dsl.Example("The service is unavailable.") })
	dsl.Required("code", "message")
})

// NotFoundError is the DSL type for a 404.
var NotFoundError = dsl.Type("NotFoundError", func() {
	dsl.Attribute("code", dsl.String, "HTTP status code", func() { dsl.Example("404") })
	dsl.Attribute("message", dsl.String, "Error message")
	dsl.Required("code", "message")
})

// UnauthorizedError is the DSL type for a 401. Declaring this as a named
// error (rather than letting JWTAuth's failure fall through as a bare Go
// error) is required for Goa to encode it as 401: an error that isn't one
// of a method's declared error types is encoded via the transport's default
// formatter, which reports 500 regardless of the failure's actual cause.
var UnauthorizedError = dsl.Type("UnauthorizedError", func() {
	dsl.Attribute("code", dsl.String, "HTTP status code", func() { dsl.Example("401") })
	dsl.Attribute("message", dsl.String, "Error message")
	dsl.Required("code", "message")
})

// FormationError is the shared error shape for the write endpoints. The UI
// switches on reason, never on the HTTP status alone: several reasons share
// one status (checklist_read_only and invalid_transition are both 409), so
// status is not enough to tell them apart.
var FormationError = dsl.Type("FormationError", func() {
	// One update_item call declares four Error()s (NotFound,
	// VersionMismatch, Conflict, BadRequest) all typed as FormationError,
	// so Goa needs a field that tells the four apart to pick the right
	// HTTP status. This is that field: the service sets it to the
	// error's declared name (e.g. "Conflict"). It is not what the UI
	// should switch on — that is reason, below, which is one level more
	// specific (several reasons share one name/status).
	dsl.ErrorName("name", dsl.String, "Which declared error this is — matches the Error() name (e.g. \"Conflict\"). Transport dispatch only; switch on reason, not this.")
	dsl.Attribute("code", dsl.String, "HTTP status code", func() { dsl.Example("409") })
	dsl.Attribute("message", dsl.String, "Human-readable message")
	dsl.Attribute("reason", dsl.String, "Machine-readable; switch on this, not on status.", func() {
		dsl.Enum(
			"not_found",
			"version_mismatch",
			"unknown_item_key",
			"checklist_read_only",
			"invalid_transition",
			"blocked_reason_required",
			"skip_reason_required",
			"return_reason_required",
			"assignee_not_on_project",
			"link_scheme_invalid",
			"due_date_invalid",
			"sub_item_null",
			"unknown_sub_item_key",
			"no_fields_to_update",
		)
	})
	dsl.Required("name", "code", "message", "reason")
})

// ApplicationError is the error shape for the application routes.
//
// Separate from FormationError rather than sharing it: the two have disjoint
// reason sets, and a shared enum would publish checklist reasons like
// skip_reason_required on a route that can never produce one, inviting a
// client to handle cases that do not exist and to treat the shared type as a
// promise that they do.
var ApplicationError = dsl.Type("ApplicationError", func() {
	dsl.ErrorName("name", dsl.String, "Which declared error this is — matches the Error() name. Transport dispatch only; switch on reason, not this.")
	dsl.Attribute("code", dsl.String, "HTTP status code", func() { dsl.Example("400") })
	dsl.Attribute("message", dsl.String, "Human-readable message")
	dsl.Attribute("reason", dsl.String, "Machine-readable; switch on this, not on status.", func() {
		dsl.Enum(
			"not_found",
			"application_uid_invalid",
			"version_mismatch",
			"submitter_username_required",
			"project_website_invalid",
			"formation_list_invalid",
		)
	})
	dsl.Required("name", "code", "message", "reason")
})

// ProjectApplication is one application as this service returns it.
//
// The submitter's name and email are on the wire because the routes that
// return this are all reached through a relation on the application itself,
// so every caller is either the submitter or the formation team. Nothing here
// is readable on the public wildcard.
//
// No project_uid field, in either direction. An application belongs to no
// project while under review, and nothing writes the created project's UID
// back after acceptance — so "accepted and created" and "accepted, never
// created" are indistinguishable from here. That is an accepted consequence
// of the agreed hand-off, not an omission to be fixed by adding a field.
var ProjectApplication = dsl.ResultType("application/vnd.project.application+json", "ProjectApplication", func() {
	dsl.Attribute("uid", dsl.String, "The application's UID. The FGA object id and the indexed document id are both this value.")
	// No dsl.Enum. Only accepted and denied are agreed names; the rest are
	// this service's own, and publishing them as a closed set in the OpenAPI
	// document would invite a client to reject a value it has not seen the
	// first time one is added.
	dsl.Attribute("state", dsl.String, "Where the application stands. accepted and denied are the two decided outcomes.", func() {
		dsl.Example("submitted")
	})
	dsl.Attribute("revision", dsl.Int64, "Echo as If-Match on every mutation.")
	dsl.Attribute("submitter_username", dsl.String)
	dsl.Attribute("submitter_name", dsl.String)
	dsl.Attribute("submitter_email", dsl.String)
	dsl.Attribute("target_parent_uid", dsl.String, "Absent unless the applicant started from somewhere. A hint, never a placement.")
	dsl.Attribute("application", dsl.MapOf(dsl.String, dsl.Any), "The intake answers, as submitted.")
	dsl.Attribute("created_at", dsl.String, func() { dsl.Format(dsl.FormatDateTime) })
	dsl.Attribute("updated_at", dsl.String, func() { dsl.Format(dsl.FormatDateTime) })
	dsl.Required("uid", "state", "revision", "submitter_username", "submitter_name", "submitter_email", "application", "created_at", "updated_at")
})

var ProjectApplicationMutationResult = dsl.ResultType(
	"application/vnd.project.application.mutation+json",
	"ProjectApplicationMutationResult",
	func() {
		dsl.Attribute("application", ProjectApplication)
		ApplicationETagAttribute()
		dsl.Required("application")
	},
)

// FormationSubItem is informational detail on a checklist item. The parent's
// status is never derived from these.
var FormationSubItem = dsl.Type("FormationSubItem", func() {
	dsl.Attribute("key", dsl.String)
	dsl.Attribute("title", dsl.String)
	dsl.Attribute("status", dsl.String, func() {
		dsl.Enum("not_started", "in_progress", "blocked", "done", "skipped")
	})
	dsl.Required("key", "title", "status")
})

// FormationSubItemUpdate is the input shape for updating a sub-item's
// status. Unlike FormationSubItem (the read shape), it carries no title —
// sub-items are copied from the template and immutable except for status.
var FormationSubItemUpdate = dsl.Type("FormationSubItemUpdate", func() {
	dsl.Attribute("key", dsl.String)
	dsl.Attribute("status", dsl.String, func() {
		dsl.Enum("not_started", "in_progress", "blocked", "done", "skipped")
	})
	dsl.Required("key", "status")
})

// FormationPlatformCheck describes how a platform-sourced item is resolved.
var FormationPlatformCheck = dsl.Type("FormationPlatformCheck", func() {
	dsl.Attribute("resource_type", dsl.String)
	dsl.Attribute("min_count", dsl.Int)
})

// FormationResolvedRef is what a platform check found.
var FormationResolvedRef = dsl.Type("FormationResolvedRef", func() {
	dsl.Attribute("type", dsl.String)
	dsl.Attribute("uid", dsl.String)
})

// FormationItem is one checklist row. Everything about how the row *looks* —
// labels, icons, the composed detail line — belongs to the browser; this
// carries only functional fields, keyed on item_key, so presentation can
// change without a contract change.
var FormationItem = dsl.Type("FormationItem", func() {
	dsl.Attribute("uid", dsl.String)
	dsl.Attribute("item_key", dsl.String, "Stable identifier, e.g. charter_agreed. Never changes.")
	dsl.Attribute("section_key", dsl.String)
	dsl.Attribute("position", dsl.Int)
	dsl.Attribute("title", dsl.String)
	dsl.Attribute("owner_team", dsl.String, func() { dsl.Example("formation") })
	dsl.Attribute("gate", dsl.Boolean, "Whether this item blocks going live.")
	dsl.Attribute("requires_writer", dsl.Boolean)
	dsl.Attribute("status_source", dsl.String, func() { dsl.Enum("manual", "platform") })
	dsl.Attribute("is_required", dsl.Boolean, "Whether this item must be filled in. Display metadata, not a gate.")
	dsl.Attribute("checklist_type", dsl.String, "Which audience this item is for. Display metadata only; the response is never filtered by it.", func() {
		dsl.Enum("internal", "external", "both")
	})
	dsl.Attribute("platform_check", FormationPlatformCheck)
	dsl.Attribute("action_link", dsl.String, "Placeholders substituted once at expansion.")
	dsl.Attribute("evidence_link", dsl.String, "Writer-set; feeds Quick Links.", func() {
		dsl.Example("https://example.org/bylaws.pdf")
	})
	dsl.Attribute("status", dsl.String, "Five values.", func() {
		dsl.Enum("not_started", "in_progress", "blocked", "done", "skipped")
	})
	dsl.Attribute("assignee", dsl.String, "Username. Nothing is granted.")
	dsl.Attribute("due_date", dsl.String, func() { dsl.Format(dsl.FormatDate) })
	dsl.Attribute("note", dsl.String)
	dsl.Attribute("skip_reason", dsl.String, "Required when status is skipped.")
	dsl.Attribute("resolved_ref", FormationResolvedRef, "Set by the service.")
	dsl.Attribute("sub_items", dsl.ArrayOf(FormationSubItem))
	dsl.Attribute("available_actions", dsl.ArrayOf(FormationAvailableAction),
		"What this item's current state permits, and what each action requires. Describes the "+
			"item, not the caller: two people reading the same item receive the same list, and a "+
			"browser intersects it with the standing it already holds. Empty, never absent, when "+
			"the item permits nothing.")
	dsl.Attribute("version", dsl.Int64, "Echo as If-Match on every mutation. Per item, not per formation.")
	dsl.Required("uid", "item_key", "section_key", "position", "title", "gate", "requires_writer", "status_source", "is_required", "checklist_type", "status", "available_actions", "version")
})

// FormationAvailableAction is one thing that may be done to an item in its
// current state, by somebody holding the stated relation.
//
// Deliberately no dsl.Enum on action or requires_relation. An enum in a
// published document invites a consumer to validate against it and reject a
// value it has not seen, and both sets are expected to grow — the relation
// names in particular move with the gateway's guards. A consumer meeting an
// unrecognised value must be able to ignore that entry and carry on, which an
// enum would turn into a decode failure.
//
// No label, icon, description or ordering hint: wording stays in the browser,
// keyed on the stable name.
var FormationAvailableAction = dsl.Type("FormationAvailableAction", func() {
	dsl.Attribute("action", dsl.String, "Stable identifier, never display text.", func() {
		dsl.Example("mark_done")
	})
	dsl.Attribute("requires_reason", dsl.Boolean,
		"Whether taking this action must carry a reason or note, so a browser can render the "+
			"input without knowing which actions need one.")
	dsl.Attribute("requires_relation", dsl.String,
		"What the caller must hold for the gateway to admit the call — a relation on the project, "+
			"or a team membership. Names a guard the deployed rules already publish; it discloses "+
			"nothing about the caller.", func() {
			dsl.Example("writer")
		})
	dsl.Required("action", "requires_reason", "requires_relation")
})

// FormationSection groups items under one heading.
var FormationSection = dsl.Type("FormationSection", func() {
	dsl.Attribute("key", dsl.String)
	dsl.Attribute("title", dsl.String)
	dsl.Attribute("position", dsl.Int)
	dsl.Required("key", "title", "position")
})

// FormationProgress is the five-way status count, derived on every read and
// never stored — skipped is its own bucket, never folded into done.
var FormationProgress = dsl.Type("FormationProgress", func() {
	dsl.Attribute("not_started", dsl.Int)
	dsl.Attribute("in_progress", dsl.Int)
	dsl.Attribute("blocked", dsl.Int)
	dsl.Attribute("done", dsl.Int)
	dsl.Attribute("skipped", dsl.Int)
	dsl.Required("not_started", "in_progress", "blocked", "done", "skipped")
})

// FormationChecklist is the response body for GET /formations/{project_uid}
// — one response for the whole page.
var FormationChecklist = dsl.ResultType("application/vnd.formation.checklist+json", "FormationChecklist", func() {
	dsl.Attribute("project_uid", dsl.String)
	dsl.Attribute("template_uid", dsl.String)
	dsl.Attribute("template_version", dsl.Int)
	dsl.Attribute("lifecycle", dsl.String, func() { dsl.Enum("live", "completed", "frozen") })
	dsl.Attribute("sections", dsl.ArrayOf(FormationSection))
	dsl.Attribute("items", dsl.ArrayOf(FormationItem))
	dsl.Attribute("progress", FormationProgress)
	dsl.Attribute("is_activating", dsl.Boolean, "Every gating item done, at least one gating item exists, and the project has an announcement date.")
	dsl.Required("project_uid", "template_uid", "template_version", "lifecycle", "sections", "items", "progress", "is_activating")
	// No explicit default view: Goa generates an all-attribute one, and
	// hand-listing the attributes only creates a place to forget a new one,
	// which would silently drop it from the wire.
})

// FormationActivityEntry is one immutable record in the checklist's feed.
var FormationActivityEntry = dsl.Type("FormationActivityEntry", func() {
	dsl.Attribute("ulid", dsl.String, "Time-ordered; doubles as the paging cursor.")
	// Absent has two meanings and a consumer must not read it as only the
	// first: the second case is not reachable today (nothing deletes an item)
	// but the column is ON DELETE SET NULL, so an entry can outlive its item.
	dsl.Attribute("item_uid", dsl.String,
		"Nullable. Absent means either a formation-level entry — template expansion or upgrade, which "+
			"concern no single item — or an entry whose item has since been removed, since the reference "+
			"is cleared rather than the row deleted. The two are indistinguishable here. Neither is ever "+
			"returned by a read filtered on item_uid.")
	dsl.Attribute("actor", dsl.String)
	dsl.Attribute("set_by", dsl.String, func() { dsl.Enum("user", "system") })
	dsl.Attribute("action", dsl.String)
	dsl.Attribute("before", dsl.Any, "Redacted summary.")
	dsl.Attribute("after", dsl.Any)
	dsl.Attribute("at", dsl.String, func() { dsl.Format(dsl.FormatDateTime) })
	dsl.Required("ulid", "actor", "set_by", "action", "at")
})

// FormationActivityPage is the response body for
// GET /formations/{project_uid}/activity.
var FormationActivityPage = dsl.ResultType("application/vnd.formation.activity+json", "FormationActivityPage", func() {
	dsl.Attribute("entries", dsl.ArrayOf(FormationActivityEntry))
	dsl.Attribute("next_cursor", dsl.String, "Pass as cursor to fetch the next page. Empty on the last page.")
	dsl.Required("entries")
})
