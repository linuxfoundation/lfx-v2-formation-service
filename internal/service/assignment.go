// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// validateAssignee refuses assigning to someone who holds no direct grant
// (writer or auditor) on the project. Assignment itself is data only and
// grants nothing on its own — this only catches assigning to someone with
// no standing on the project at all, e.g. a typo'd username.
//
// Live as of the settings read being wired: both halves of the roster come from
// the one record, so an auditor is recognised rather than refused for being
// absent from the writers list. It was inert before that, because answering with
// writers alone would have rejected every legitimate auditor.
//
// A nil ProjectReader skips the check rather than refusing every assignment,
// which is the same conservative handling GetFormation gives this dependency for
// the announcement date. Reaching that branch now means NATS was unreachable at
// startup, and skipping is the right direction: this check catches a typo'd
// username, and an upstream outage is no reason to refuse assignments outright.
//
// Only a roster that was actually read refuses. Every failure to read one —
// including the empty reply that would otherwise read as "this project has no
// settings" — allows the assignment through instead.
//
// That is not caution for its own sake: the project service answers every handler
// failure with an empty reply, distinguished only by the level it logs at, so a
// KV read error, an uninitialised store during startup and a genuinely absent
// settings record are the same three zero bytes on the wire. Refusing on that
// would answer a transient upstream fault with assignee_not_on_project — telling
// someone their colleague is not on the project, which is both wrong and
// unactionable. Since this check exists to catch a typo'd username and assignment
// grants nothing on its own, allowing on a failed read costs a typo getting
// through until the roster is readable again.
//
// # Why an auditor is assignable but cannot then work the item
//
// Accepting auditors here admits someone the gateway will refuse: View maps to
// the project's auditor grant and Manage to writer, and every mutation on this
// route is guarded on writer, so an auditor named as assignee cannot PATCH their
// own item — not even to claim it complete.
//
// That asymmetry is intended, and it is not resolved by relaxing the guard to
// writer-or-assignee. Doing so would create someone who can tick "mailing list
// created" while holding no access to create a mailing list; the checklist would
// record work that the same person could not have done. Assignment is a Manage
// action taken on an auditor's behalf, and the resolution when an auditor should
// actually do the work is to raise their access to Manage first — a decision for
// whoever is assigning, which is why it belongs in the assigning UI and not in a
// second gateway rule here.
//
// Auditors are still accepted rather than refused, because the roster is the
// test of whether the assignee has standing on the project at all, which is what
// this function is for. Refusing them would reject people the People panel lists.
//
// UpdateItem calls this before opening its transaction, so this round-trip does
// not hold a row lock for its duration, and carries the result into the
// transaction to report from the position the check used to occupy. The rationale
// for both halves is at that call site.
func validateAssignee(ctx context.Context, projects port.ProjectReader, projectUID, assignee string) error {
	if projects == nil {
		return nil
	}

	settings, err := projects.GetSettings(ctx, projectUID)
	if err != nil {
		// Logged rather than propagated, and at WARN: an unreadable roster is an
		// upstream condition, not a fault in this request, and failing the PATCH
		// over it would block the item update itself rather than only the check.
		slog.WarnContext(ctx, "could not read the project roster; accepting the assignee unchecked",
			"project_uid", projectUID, "assignee", assignee, "error", err)
		return nil
	}
	if settings == nil {
		slog.WarnContext(ctx, "project roster read returned nothing; accepting the assignee unchecked",
			"project_uid", projectUID, "assignee", assignee)
		return nil
	}

	for _, w := range settings.Writers {
		if w == assignee {
			return nil
		}
	}
	for _, a := range settings.Auditors {
		if a == assignee {
			return nil
		}
	}
	return domain.NewReasonError(domain.ErrInvalidRequest, reasonAssigneeNotOnProject)
}
