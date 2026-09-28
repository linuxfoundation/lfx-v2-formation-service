// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// applicationEmailService extends applicationService with a wired emailer and
// email config so the dispatch helpers can run.
func applicationEmailService(t *testing.T) (applicationDoubles, *mock.EmailDispatcher) {
	t.Helper()
	d := applicationService(t)
	mailer := mock.NewEmailDispatcher()
	d.service.emailer = mailer
	d.service.emailCfg = EmailConfig{
		Enabled:        true,
		FormationInbox: "formation@example.test",
		AdminBaseURL:   "https://app.lfx.dev",
	}
	return d, mailer
}

// --- submit emails ---

func TestApplicationSubmittedSendsReceiptToSubmitter(t *testing.T) {
	d, mailer := applicationEmailService(t)

	_, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)

	sent := mailer.Sent()
	var receipt string
	for _, m := range sent {
		if m.To == "asmith@example.test" {
			receipt = m.Subject
			break
		}
	}
	assert.NotEmpty(t, receipt, "expected a receipt email to the submitter")
	assert.Contains(t, receipt, "Proposed Project", "subject must name the project")
}

func TestApplicationSubmittedSendsTeamAlert(t *testing.T) {
	d, mailer := applicationEmailService(t)

	_, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)

	sent := mailer.Sent()
	var teamMsg string
	for _, m := range sent {
		if m.To == "formation@example.test" {
			teamMsg = m.Subject
			break
		}
	}
	assert.NotEmpty(t, teamMsg, "expected an alert to the formation team inbox")
	assert.Contains(t, teamMsg, "Proposed Project", "subject must name the project")
}

func TestApplicationSubmittedSendsNoTeamAlertWhenInboxEmpty(t *testing.T) {
	d, mailer := applicationEmailService(t)
	d.service.emailCfg.FormationInbox = ""

	_, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)

	for _, m := range mailer.Sent() {
		assert.NotEqual(t, "formation@example.test", m.To,
			"no team alert expected when formation inbox is not configured")
	}
}

func TestApplicationSubmittedIdempotent(t *testing.T) {
	// The mock's MarkApplicationNotified returns false on a second call for the
	// same column, so a duplicate submit (if it were possible) would not send a
	// second receipt.
	d, mailer := applicationEmailService(t)
	result, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)

	// Manually mark the submitted column as already sent and fire again.
	uid := mustUUID(t, result.UID)
	acquired, err := d.applications.MarkApplicationNotified(t.Context(), uid, "notified_submitted_at")
	require.NoError(t, err)
	assert.False(t, acquired, "second mark must return acquired=false")

	// Count how many emails were sent to the submitter.
	count := 0
	for _, m := range mailer.Sent() {
		if m.To == "asmith@example.test" {
			count++
		}
	}
	assert.Equal(t, 1, count, "submitter must receive exactly one receipt even when mark is called twice")
}

// --- accepted email ---

func TestApplicationAcceptedSendsEmail(t *testing.T) {
	d, mailer := applicationEmailService(t)
	created := submitOne(t, d.service)
	mailer.Reset() // clear the submit emails

	_, err := d.service.AcceptApplication(asPrincipal("reviewer"), &svc.AcceptApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, mailer.SentCount(), "expected one accepted email")
	sent := mailer.Sent()[0]
	assert.Equal(t, "asmith@example.test", sent.To)
	assert.Contains(t, sent.Subject, "Proposed Project")
}

func TestApplicationAcceptedIdempotent(t *testing.T) {
	// Accepting twice (different reviewers, both succeed) must send only one
	// email because MarkApplicationNotified returns false on the second call.
	d, mailer := applicationEmailService(t)
	created := submitOne(t, d.service)
	mailer.Reset()

	_, err := d.service.AcceptApplication(asPrincipal("reviewer-one"), &svc.AcceptApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})
	require.NoError(t, err)

	// Pre-mark the column so the second accept finds acquired=false.
	uid := mustUUID(t, created.UID)
	d.applications.MarkApplicationNotified(t.Context(), uid, "notified_accepted_at") //nolint:errcheck

	assert.Equal(t, 1, mailer.SentCount(), "only one accepted email regardless of how many times the column is checked")
}

// --- denied email ---

func TestApplicationDeniedSendsEmail(t *testing.T) {
	d, mailer := applicationEmailService(t)
	created := submitOne(t, d.service)
	mailer.Reset()

	_, err := d.service.DenyApplication(asPrincipal("reviewer"), &svc.DenyApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, mailer.SentCount(), "expected one denied email")
	sent := mailer.Sent()[0]
	assert.Equal(t, "asmith@example.test", sent.To)
	assert.Contains(t, sent.Subject, "Proposed Project")
}

// --- nil emailer / disabled degradation ---

func TestApplicationEmailsNotSentWhenEmailerNil(t *testing.T) {
	d := applicationService(t)
	d.service.emailCfg = EmailConfig{Enabled: true, FormationInbox: "formation@example.test"}
	// emailer deliberately left nil

	_, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)
	// No panic, no emails — nil emailer is handled gracefully.
}

func TestApplicationEmailsNotSentWhenDisabled(t *testing.T) {
	d, mailer := applicationEmailService(t)
	d.service.emailCfg.Enabled = false

	_, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)

	assert.Equal(t, 0, mailer.SentCount(), "no emails expected when email is disabled")
}

func TestApplicationDecisionEmailsNotSentWhenDisabled(t *testing.T) {
	d, mailer := applicationEmailService(t)
	created := submitOne(t, d.service)
	d.service.emailCfg.Enabled = false
	mailer.Reset()

	_, err := d.service.AcceptApplication(asPrincipal("reviewer"), &svc.AcceptApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})
	require.NoError(t, err)

	assert.Equal(t, 0, mailer.SentCount(), "no emails expected when email is disabled")
}

// --- email content ---

func TestApplicationSubmittedEmailContent(t *testing.T) {
	d, mailer := applicationEmailService(t)

	_, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)

	var submitterMsg *port.EmailMessage
	for i, m := range mailer.Sent() {
		if m.To == "asmith@example.test" {
			msg := mailer.Sent()[i]
			submitterMsg = &msg
			break
		}
	}
	require.NotNil(t, submitterMsg)
	assert.Contains(t, submitterMsg.HTML, "Proposed Project", "HTML must include the project name")
	assert.Contains(t, submitterMsg.Text, "Proposed Project", "plain text must include the project name")
	assert.Contains(t, submitterMsg.HTML, "A Smith", "HTML must greet the submitter by name")
	assert.Contains(t, submitterMsg.HTML, "formation@example.test", "HTML must include the formation team contact address")
}

func TestApplicationSubmittedTeamEmailContent(t *testing.T) {
	d, mailer := applicationEmailService(t)

	_, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)

	var teamMsg *port.EmailMessage
	for i, m := range mailer.Sent() {
		if m.To == "formation@example.test" {
			msg := mailer.Sent()[i]
			teamMsg = &msg
			break
		}
	}
	require.NotNil(t, teamMsg)
	assert.Contains(t, teamMsg.HTML, "Proposed Project")
	assert.Contains(t, teamMsg.HTML, "asmith@example.test", "team alert must include the submitter's email")
}

