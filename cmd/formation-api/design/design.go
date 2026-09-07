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

var _ = dsl.Service("lfx_v2_formation_service", func() {
	dsl.Description("LFX V2 Formation Service")

	dsl.Method("get_formation", func() {
		dsl.Description("Return the whole checklist for a project in one response — sections, items, progress and readiness. Items are never fetched individually.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			dsl.Attribute("project_uid", dsl.String, "The project's UID.")
			dsl.Required("project_uid")
		})
		dsl.Result(FormationChecklist)
		dsl.Error("NotFound", NotFoundError, "No formation exists for this project")
		dsl.HTTP(func() {
			dsl.GET("/formations/{project_uid}")
			dsl.Header("bearer_token:Authorization")
			dsl.Response(dsl.StatusOK)
			dsl.Response("NotFound", dsl.StatusNotFound)
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
			dsl.Attribute("project_uid", dsl.String, "The project's UID.")
			dsl.Attribute("cursor", dsl.String, "Opaque ULID cursor from a previous page's next_cursor. Omit for the first page.")
			dsl.Attribute("limit", dsl.Int, "Page size. Defaults to 20, capped at 100.", func() {
				dsl.Default(20)
			})
			dsl.Required("project_uid")
		})
		dsl.Result(FormationActivityPage)
		dsl.Error("NotFound", NotFoundError, "No formation exists for this project")
		dsl.HTTP(func() {
			dsl.GET("/formations/{project_uid}/activity")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("cursor")
			dsl.Param("limit")
			dsl.Response(dsl.StatusOK)
			dsl.Response("NotFound", dsl.StatusNotFound)
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
// carries only functional fields, keyed on item_key (endpoints.md, "What the
// UI puts on a row").
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
	dsl.Attribute("is_activating", dsl.Boolean, "Every gating item done, and at least one gating item exists.")
	dsl.Required("project_uid", "template_uid", "template_version", "lifecycle", "sections", "items", "progress", "is_activating")
	dsl.View("default", func() {
		dsl.Attribute("project_uid")
		dsl.Attribute("template_uid")
		dsl.Attribute("template_version")
		dsl.Attribute("lifecycle")
		dsl.Attribute("sections")
		dsl.Attribute("items")
		dsl.Attribute("progress")
		dsl.Attribute("is_activating")
	})
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
	dsl.View("default", func() {
		dsl.Attribute("entries")
		dsl.Attribute("next_cursor")
	})
})
