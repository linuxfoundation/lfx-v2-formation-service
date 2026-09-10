// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	emailapi "github.com/linuxfoundation/lfx-v2-email-service/pkg/api"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// EmailDispatcher publishes outbound formation notification emails to the
// email service over NATS request/reply.
//
// It satisfies port.EmailDispatcher. A nil client means NATS was not
// available at startup; the dispatcher degrades by logging and returning nil
// so callers are not forced to gate on the emailer being wired.
type EmailDispatcher struct {
	client *Client
}

// Compile-time check.
var _ port.EmailDispatcher = (*EmailDispatcher)(nil)

// NewEmailDispatcher wires an email dispatcher over the shared NATS client.
func NewEmailDispatcher(client *Client) *EmailDispatcher {
	return &EmailDispatcher{client: client}
}

// Send publishes one email request to the email service and waits for the
// reply. A failure is logged and returned but must not block the caller's
// own write.
func (d *EmailDispatcher) Send(ctx context.Context, msg port.EmailMessage) error {
	if d.client == nil {
		slog.WarnContext(ctx, "email dispatcher: NATS client not available; email not sent",
			"to", msg.To, "subject", msg.Subject)
		return nil
	}

	req := emailapi.SendEmailRequest{
		To:      msg.To,
		Subject: msg.Subject,
		HTML:    msg.HTML,
		Text:    msg.Text,
		GroupID: msg.GroupID,
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("email dispatcher: marshal send request: %w", err)
	}

	reply, err := d.client.Request(ctx, emailapi.SendEmailSubject, payload)
	if err != nil {
		return fmt.Errorf("email dispatcher: NATS request to email service: %w", err)
	}

	// A non-empty reply body that starts with `{"error"` is an error response
	// from the email service. Log it and surface it as an error so the caller
	// can decide whether to mark the notification as sent or retry.
	if len(reply) > 0 {
		var errResp emailapi.SendEmailErrorResponse
		if json.Unmarshal(reply, &errResp) == nil && errResp.Error != "" {
			return fmt.Errorf("email service rejected send: %s", errResp.Error)
		}
	}

	return nil
}
