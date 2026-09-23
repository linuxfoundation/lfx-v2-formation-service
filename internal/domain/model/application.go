// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// ApplicationState is one project application's position in review.
//
// Only two values are named by the design this implements, and both are
// outcomes: an application is accepted or it is denied. What it is called
// before either happens, and what withdrawing leaves behind, are not settled
// anywhere, so they are named here as this service's own choices rather than
// presented as part of the agreed vocabulary.
//
// Decision states use the review's accepted and denied vocabulary directly;
// aliases would become distinct states once clients switch on the wire value.
type ApplicationState string

const (
	// ApplicationSubmitted is an application awaiting a decision.
	//
	// This service's name for it. The governing design names no state here,
	// so nothing outside this repository depends on the spelling.
	ApplicationSubmitted ApplicationState = "submitted"

	// ApplicationWithdrawn is an application withdrawn from review.
	// This is the service's name; withdraw is an agreed action, but the state
	// it leaves behind was never given one.
	ApplicationWithdrawn ApplicationState = "withdrawn"

	// ApplicationAccepted is agreed vocabulary. Accepting records the
	// decision and hands off to an ordinary staff project create; this
	// service does not create the project.
	ApplicationAccepted ApplicationState = "accepted"

	// ApplicationDenied is agreed vocabulary. Denying keeps the record for
	// audit and re-application history, and leaves nothing to delete,
	// because nothing was created.
	ApplicationDenied ApplicationState = "denied"
)

// Application is a proposal to start a new foundation, made before any project
// exists to hold it.
//
// It is not a draft project and not a variant of Formation. A Formation is
// keyed by project_uid and every one of its authorization checks resolves
// through that project; an application has no project, sits nowhere in the
// project tree, and belongs to no incorporated entity at all — which is its
// normal state under review and an unrepresentable one for a project. That is
// the whole reason it is a separate type: making it a project early would
// force a placement decision the reviewer has not made yet.
type Application struct {
	bun.BaseModel `bun:"table:project_applications,alias:pa"`

	UID      uuid.UUID        `bun:"uid,pk,default:gen_random_uuid()"`
	State    ApplicationState `bun:"state,notnull"`
	Revision int64            `bun:"revision,notnull,default:1"`

	// The submitter as recorded data, not as an authenticated identity.
	//
	// The UI creates the application authenticating as itself, so the end user
	// never presents a credential to this service and nothing here attests
	// these three fields. They are what the caller said, which is why the
	// caller's own team membership is the only thing the gateway can check.
	SubmitterUsername string `bun:"submitter_username,notnull"`
	SubmitterName     string `bun:"submitter_name,notnull"`
	SubmitterEmail    string `bun:"submitter_email,notnull"`

	// TargetParentUID is a hint prefilled from wherever the submitter started,
	// and nothing more. It never decides the incorporated entity — that
	// question is deliberately not asked at intake and belongs to whoever
	// approves — and it grants nobody anything, since no authorization check
	// in this feature reads it. Usually absent.
	TargetParentUID *string `bun:"target_parent_uid"`

	// Payload is the intake answers, held whole rather than as columns.
	//
	// The source names the fields but does not define wire keys, types or
	// requiredness. Keeping them together avoids inventing a typed schema.
	//
	// Two contents are worth knowing about from here. People the submitter
	// names for the formation work are email addresses only: this service
	// never resolves them to platform identities, never grants them anything
	// and never notifies them. And the proposed project's website is carried
	// as a URL value like every other answer.
	Payload map[string]any `bun:"payload,type:jsonb,notnull"`

	CreatedAt time.Time `bun:"created_at,nullzero,notnull,default:now()"`
	UpdatedAt time.Time `bun:"updated_at,nullzero,notnull,default:now()"`
}

// ApplicationDeletion is the durable, PII-free repair marker left after an
// application is removed.
type ApplicationDeletion struct {
	bun.BaseModel `bun:"table:project_application_deletions,alias:pad"`

	UID       uuid.UUID `bun:"uid,pk"`
	Revision  int64     `bun:"revision,notnull"`
	DeletedAt time.Time `bun:"deleted_at,nullzero,notnull,default:now()"`
}
