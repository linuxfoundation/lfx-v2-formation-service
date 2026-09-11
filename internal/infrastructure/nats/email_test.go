// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	emailapi "github.com/linuxfoundation/lfx-v2-email-service/pkg/api"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// sampleEmailMessage returns a representative EmailMessage for dispatcher tests.
func sampleEmailMessage() port.EmailMessage {
	return port.EmailMessage{
		To:      "alice@example.com",
		Subject: "You've been assigned: Charter agreed",
		HTML:    "<p>You have a new item assigned.</p>",
		Text:    "You have a new item assigned.",
		GroupID: "formation.item_assigned",
	}
}

// TestEmailDispatcherSendsToTheCorrectSubject ensures the adapter publishes to
// the email service's declared NATS subject. A wrong subject is silently
// unrouted in production — no error, just no email.
func TestEmailDispatcherSendsToTheCorrectSubject(t *testing.T) {
	url := startTestNATSServer(t)

	// Capture which subject the request lands on so we can assert it.
	var capturedSubject string
	respondOn(t, url, emailapi.SendEmailSubject, func(data string) []byte {
		capturedSubject = emailapi.SendEmailSubject
		return []byte("{}")
	})

	d := NewEmailDispatcher(newTestClient(t, url, 2*time.Second))
	if err := d.Send(context.Background(), sampleEmailMessage()); err != nil {
		t.Fatalf("Send() = %v, want no error", err)
	}

	if capturedSubject != emailapi.SendEmailSubject {
		t.Errorf("message published to %q, want %q", capturedSubject, emailapi.SendEmailSubject)
	}
}

// TestEmailDispatcherSerializesTheRequest verifies that the adapter correctly
// marshals the EmailMessage fields into the JSON payload the email service
// expects. A field that survives the port.EmailMessage but vanishes in
// SendEmailRequest would silently drop it.
func TestEmailDispatcherSerializesTheRequest(t *testing.T) {
	url := startTestNATSServer(t)
	msg := sampleEmailMessage()

	var received []byte
	respondOn(t, url, emailapi.SendEmailSubject, func(data string) []byte {
		received = []byte(data)
		return []byte("{}")
	})

	d := NewEmailDispatcher(newTestClient(t, url, 2*time.Second))
	if err := d.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send() = %v, want no error", err)
	}

	var req emailapi.SendEmailRequest
	if err := json.Unmarshal(received, &req); err != nil {
		t.Fatalf("decoding received payload: %v", err)
	}
	if req.To != msg.To {
		t.Errorf("To = %q, want %q", req.To, msg.To)
	}
	if req.Subject != msg.Subject {
		t.Errorf("Subject = %q, want %q", req.Subject, msg.Subject)
	}
	if req.HTML != msg.HTML {
		t.Errorf("HTML = %q, want %q", req.HTML, msg.HTML)
	}
	if req.Text != msg.Text {
		t.Errorf("Text = %q, want %q", req.Text, msg.Text)
	}
	if req.GroupID != msg.GroupID {
		t.Errorf("GroupID = %q, want %q", req.GroupID, msg.GroupID)
	}
}

// TestEmailDispatcherReturnsErrorOnSendEmailErrorResponse covers the
// error-response branch (email.go:65–70): a reply body containing
// `{"error":"..."}` must surface as an error so the caller knows the
// email was rejected rather than silently succeeding.
func TestEmailDispatcherReturnsErrorOnSendEmailErrorResponse(t *testing.T) {
	url := startTestNATSServer(t)
	const rejectMsg = "recipient address is invalid"

	respondOn(t, url, emailapi.SendEmailSubject, func(string) []byte {
		errResp, _ := json.Marshal(emailapi.SendEmailErrorResponse{Error: rejectMsg})
		return errResp
	})

	d := NewEmailDispatcher(newTestClient(t, url, 2*time.Second))
	err := d.Send(context.Background(), sampleEmailMessage())
	if err == nil {
		t.Fatal("Send() with error reply = nil, want an error")
	}
	if !strings.Contains(err.Error(), rejectMsg) {
		t.Errorf("error = %q, want it to include the rejection message %q", err, rejectMsg)
	}
}

// TestEmailDispatcherReturnsErrorOnTransportFailure covers the NATS-error
// branch: if the Request itself fails (no reply, connection lost, timeout),
// the error is wrapped and returned rather than swallowed.
func TestEmailDispatcherReturnsErrorOnTransportFailure(t *testing.T) {
	url := startTestNATSServer(t)
	// No responder registered — the request will time out.
	d := NewEmailDispatcher(newTestClient(t, url, 100*time.Millisecond))

	err := d.Send(context.Background(), sampleEmailMessage())
	if err == nil {
		t.Fatal("Send() with no responder = nil, want an error")
	}
}

// TestEmailDispatcherDegradesPolitelyWhenClientIsNil ensures the dispatcher
// does not panic or return an error when the NATS client is nil (the
// service's degraded start-up state). A nil client means NATS was not
// available at startup; emails are dropped and logged, not propagated.
func TestEmailDispatcherDegradesPolitelyWhenClientIsNil(t *testing.T) {
	d := &EmailDispatcher{client: nil}

	if err := d.Send(context.Background(), sampleEmailMessage()); err != nil {
		t.Errorf("Send() with nil client = %v, want nil — degraded mode must not error", err)
	}
}
