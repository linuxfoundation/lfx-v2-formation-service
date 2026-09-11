// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import "context"

// EmailDispatcher sends transactional emails via the LFX email service.
//
// Dispatch is best-effort by design — the email service is a fire-and-forget
// relay over NATS core, so a failure here is logged and must never block the
// caller's own write. The reconcile sweep owns idempotency for notifications
// that use the formation's notification state columns to avoid re-sending on
// every pass.
//
// A nil implementation is the "not wired" state and must be handled gracefully
// by every caller: degraded mode sends no mail but does not fail the request.
type EmailDispatcher interface {
	// Send delivers one email. The caller renders the HTML and plain-text
	// bodies before calling, so the dispatcher carries no template knowledge.
	//
	// GroupID is optional. When set it groups related sends for analytics
	// in the email service's engagement tracking (one group per template
	// type per batch is the convention used by project-service).
	Send(ctx context.Context, req EmailMessage) error
}

// EmailMessage is one outbound email payload.
type EmailMessage struct {
	To      string
	Subject string
	HTML    string
	Text    string
	GroupID string // optional; empty = no group analytics
}
