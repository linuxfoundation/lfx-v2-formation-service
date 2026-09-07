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
		dsl.Error("NotFound", NotFoundError, "No formation exists for this project")
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
			"them, since each save overwrites the previous state.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("project_uid", dsl.String, "The project's UID.")
			dsl.Attribute("cursor", dsl.String, "Opaque ULID cursor from a previous page's next_cursor. Omit for the first page.")
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
		dsl.Error("NotFound", NotFoundError, "No formation exists for this project")
		dsl.Error("Unauthorized", UnauthorizedError, "Missing, expired, or malformed bearer token")
		dsl.HTTP(func() {
			dsl.GET("/formations/{project_uid}/activity")
			dsl.Param("version:v")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("cursor")
			dsl.Param("limit")
			dsl.Response(dsl.StatusOK)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("Unauthorized", dsl.StatusUnauthorized)
		})
	})

	dsl.Method("update_item", func() {
		dsl.Description("Change one checklist item: status, note, due date, skip reason, evidence link, " +
			"assignee, or sub-items. Send only the fields being changed. If-Match is required and must " +
			"equal the item's current version — a stale value means re-read and retry. This route also " +
			"carries the assignee's own completion claim (status: awaiting_acceptance), but never " +
			"acceptance, rejection or reopening, which are their own routes because the formation-team " +
			"guard on those is narrower than this route's writer guard and a Heimdall rule cannot express " +
			"that on a shared route.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("project_uid", dsl.String, "The project's UID.")
			dsl.Attribute("item_key", dsl.String, "The item's stable key.")
			dsl.Attribute("if_match", dsl.Int64, "Must equal the item's current version.")
			dsl.Attribute("status", dsl.String, "One of the six. Omit to leave unchanged.", func() {
				dsl.Enum("not_started", "in_progress", "blocked", "awaiting_acceptance", "done", "skipped")
			})
			dsl.Attribute("assignee", dsl.String, "Username, or an empty string to clear.")
			// No dsl.Format(FormatDate) here, unlike the read-side
			// due_date attribute: FormatDate is enforced at request
			// decode time, before this string ever reaches the service,
			// which would make an empty string (this field's clear
			// signal) always fail validation and give a caller no way to
			// clear a due date at all. The service validates the format
			// itself for a non-empty value instead.
			dsl.Attribute("due_date", dsl.String, "YYYY-MM-DD, or an empty string to clear.")
			dsl.Attribute("note", dsl.String)
			dsl.Attribute("skip_reason", dsl.String, "Required when status is skipped.")
			dsl.Attribute("evidence_link", dsl.String, "Writer-set; feeds Quick Links. http/https only.")
			dsl.Attribute("sub_items", dsl.ArrayOf(FormationSubItemUpdate))
			dsl.Required("version", "project_uid", "item_key", "if_match")
		})
		dsl.Result(FormationItem)
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
			dsl.Response(dsl.StatusOK)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("VersionMismatch", dsl.StatusPreconditionFailed)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
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
// one status (e.g. checklist_read_only, invalid_transition and
// self_acceptance_forbidden are all 409), so status is not enough to tell
// them apart (endpoints.md, "Errors, and two people editing at once").
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
			"self_acceptance_forbidden",
			"skip_reason_required",
			"assignee_not_on_project",
			"link_scheme_invalid",
			"due_date_invalid",
		)
	})
	dsl.Required("name", "code", "message", "reason")
})

// FormationSubItem is informational detail on a checklist item. The parent's
// status is never derived from these.
var FormationSubItem = dsl.Type("FormationSubItem", func() {
	dsl.Attribute("key", dsl.String)
	dsl.Attribute("title", dsl.String)
	dsl.Attribute("status", dsl.String, func() {
		dsl.Enum("not_started", "in_progress", "blocked", "awaiting_acceptance", "done", "skipped")
	})
	dsl.Required("key", "title", "status")
})

// FormationSubItemUpdate is the input shape for updating a sub-item's
// status. Unlike FormationSubItem (the read shape), it carries no title —
// sub-items are copied from the template and immutable except for status.
var FormationSubItemUpdate = dsl.Type("FormationSubItemUpdate", func() {
	dsl.Attribute("key", dsl.String)
	dsl.Attribute("status", dsl.String, func() {
		dsl.Enum("not_started", "in_progress", "blocked", "awaiting_acceptance", "done", "skipped")
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
	dsl.Attribute("evidence_link", dsl.String, "Writer-set; feeds Quick Links.")
	dsl.Attribute("status", dsl.String, "Six values.", func() {
		dsl.Enum("not_started", "in_progress", "blocked", "awaiting_acceptance", "done", "skipped")
	})
	dsl.Attribute("assignee", dsl.String, "Username. Nothing is granted.")
	dsl.Attribute("due_date", dsl.String, func() { dsl.Format(dsl.FormatDate) })
	dsl.Attribute("note", dsl.String)
	dsl.Attribute("skip_reason", dsl.String, "Required when status is skipped.")
	dsl.Attribute("resolved_ref", FormationResolvedRef, "Set by the service.")
	dsl.Attribute("sub_items", dsl.ArrayOf(FormationSubItem))
	dsl.Attribute("version", dsl.Int64, "Echo as If-Match on every mutation. Per item, not per formation.")
	dsl.Required("uid", "item_key", "section_key", "position", "title", "gate", "requires_writer", "status_source", "is_required", "checklist_type", "status", "version")
})

// FormationSection groups items under one heading.
var FormationSection = dsl.Type("FormationSection", func() {
	dsl.Attribute("key", dsl.String)
	dsl.Attribute("title", dsl.String)
	dsl.Attribute("position", dsl.Int)
	dsl.Required("key", "title", "position")
})

// FormationProgress is the six-way status count, derived on every read and
// never stored — skipped is its own bucket, never folded into done.
var FormationProgress = dsl.Type("FormationProgress", func() {
	dsl.Attribute("not_started", dsl.Int)
	dsl.Attribute("in_progress", dsl.Int)
	dsl.Attribute("blocked", dsl.Int)
	dsl.Attribute("awaiting_acceptance", dsl.Int)
	dsl.Attribute("done", dsl.Int)
	dsl.Attribute("skipped", dsl.Int)
	dsl.Required("not_started", "in_progress", "blocked", "awaiting_acceptance", "done", "skipped")
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
	dsl.Attribute("item_uid", dsl.String, "Nullable — absent for a formation-level entry.")
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
