// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// applicationEmailService extends applicationService with a wired emailer,
// email config and a WaitGroup so tests can synchronise with the background
// email goroutines before asserting.
func applicationEmailService(t *testing.T) (applicationDoubles, *mock.EmailDispatcher) {
	t.Helper()
	d := applicationService(t)
	mailer := mock.NewEmailDispatcher()
	wg := &sync.WaitGroup{}
	d.service.emailer = mailer
	d.service.emailDispatchWG = wg
	d.service.emailCfg = EmailConfig{
		Enabled:        true,
		FormationInbox: "formation@example.test",
		AdminBaseURL:   "https://app.lfx.dev",
	}
	return d, mailer
}

// waitEmails blocks until all background email goroutines started by the
// service have finished.
func waitEmails(d applicationDoubles) { d.service.emailDispatchWG.Wait() }

// --- submit emails ---

func TestApplicationSubmittedSendsReceiptToSubmitter(t *testing.T) {
	d, mailer := applicationEmailService(t)

	_, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)
	waitEmails(d)

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
	waitEmails(d)

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
	waitEmails(d)

	for _, m := range mailer.Sent() {
		assert.NotEqual(t, "formation@example.test", m.To,
			"no team alert expected when formation inbox is not configured")
	}
}

func TestApplicationSubmittedIdempotent(t *testing.T) {
	// Calling dispatchApplicationSubmittedEmails twice for the same application
	// must send exactly one receipt and one team alert — the second call finds
	// the claim already acquired and does nothing.
	d, mailer := applicationEmailService(t)
	result, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)
	waitEmails(d)

	// Fetch the stored application and fire the dispatcher a second time directly
	// (synchronous — no goroutine, no additional Wait needed).
	uid := mustUUID(t, result.UID)
	app, err := d.applications.Get(t.Context(), uid)
	require.NoError(t, err)
	d.service.dispatchApplicationSubmittedEmails(t.Context(), app)

	// The claim was already acquired on the first call; the second dispatch
	// must not add any new messages.
	submitter, team := 0, 0
	for _, m := range mailer.Sent() {
		switch m.To {
		case "asmith@example.test":
			submitter++
		case "formation@example.test":
			team++
		}
	}
	assert.Equal(t, 1, submitter, "submitter must receive exactly one receipt")
	assert.Equal(t, 1, team, "team must receive exactly one alert")
}

// --- accepted email ---

func TestApplicationAcceptedSendsEmail(t *testing.T) {
	d, mailer := applicationEmailService(t)
	created := submitOne(t, d.service)
	waitEmails(d)
	mailer.Reset() // clear the submit emails

	_, err := d.service.AcceptApplication(asPrincipal("reviewer"), &svc.AcceptApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})
	require.NoError(t, err)
	waitEmails(d)

	assert.Equal(t, 1, mailer.SentCount(), "expected one accepted email")
	sent := mailer.Sent()[0]
	assert.Equal(t, "asmith@example.test", sent.To)
	assert.Contains(t, sent.Subject, "Proposed Project")
}

func TestApplicationAcceptedIdempotent(t *testing.T) {
	// Calling dispatchApplicationDecidedEmail twice for the same application
	// must send exactly one accepted email — the second call finds the claim
	// already acquired and does nothing.
	d, mailer := applicationEmailService(t)
	created := submitOne(t, d.service)
	waitEmails(d)
	mailer.Reset()

	_, err := d.service.AcceptApplication(asPrincipal("reviewer-one"), &svc.AcceptApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})
	require.NoError(t, err)
	waitEmails(d)

	// Fire the dispatcher a second time directly; the claim is already held.
	d.service.dispatchApplicationDecidedEmail(t.Context(), created.UID, model.ApplicationAccepted)

	assert.Equal(t, 1, mailer.SentCount(), "submitter must receive exactly one accepted email")
}

// --- denied email ---

func TestApplicationDeniedSendsEmail(t *testing.T) {
	d, mailer := applicationEmailService(t)
	created := submitOne(t, d.service)
	waitEmails(d)
	mailer.Reset()

	_, err := d.service.DenyApplication(asPrincipal("reviewer"), &svc.DenyApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})
	require.NoError(t, err)
	waitEmails(d)

	assert.Equal(t, 1, mailer.SentCount(), "expected one denied email")
	sent := mailer.Sent()[0]
	assert.Equal(t, "asmith@example.test", sent.To)
	assert.Contains(t, sent.Subject, "Proposed Project")
}

// --- nil emailer / disabled degradation ---

func TestApplicationEmailsNotSentWhenEmailerNil(t *testing.T) {
	d := applicationService(t)
	d.service.emailCfg = EmailConfig{Enabled: true, FormationInbox: "formation@example.test"}
	// emailer deliberately left nil; emailDispatchWG also nil (no goroutine fires)

	_, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)
	// No panic, no emails — nil emailer is handled gracefully.
}

func TestApplicationEmailsNotSentWhenDisabled(t *testing.T) {
	d, mailer := applicationEmailService(t)
	d.service.emailCfg.Enabled = false

	_, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)
	waitEmails(d)

	assert.Equal(t, 0, mailer.SentCount(), "no emails expected when email is disabled")
}

func TestApplicationDecisionEmailsNotSentWhenDisabled(t *testing.T) {
	d, mailer := applicationEmailService(t)
	created := submitOne(t, d.service)
	waitEmails(d)
	d.service.emailCfg.Enabled = false
	mailer.Reset()

	_, err := d.service.AcceptApplication(asPrincipal("reviewer"), &svc.AcceptApplicationPayload{
		Version: "1", UID: created.UID, IfMatch: created.Revision,
	})
	require.NoError(t, err)
	waitEmails(d)

	assert.Equal(t, 0, mailer.SentCount(), "no emails expected when email is disabled")
}

// --- email content ---

func TestApplicationSubmittedEmailContent(t *testing.T) {
	d, mailer := applicationEmailService(t)

	_, err := d.service.CreateApplication(asPrincipal("asmith"), intakePayload())
	require.NoError(t, err)
	waitEmails(d)

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
	waitEmails(d)

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
	assert.NotContains(t, teamMsg.HTML, "asmith@example.test", "team alert must not include the submitter's email")
}
