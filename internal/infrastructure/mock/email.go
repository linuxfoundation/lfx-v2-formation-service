// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"sync"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// Compile-time check that mock satisfies the port.
var _ port.EmailDispatcher = (*EmailDispatcher)(nil)

// EmailDispatcher is an in-memory port.EmailDispatcher double. It captures
// every send so tests can assert which emails were dispatched without
// standing up a real NATS broker.
type EmailDispatcher struct {
	mu   sync.Mutex
	sent []port.EmailMessage
}

// NewEmailDispatcher constructs an empty double.
func NewEmailDispatcher() *EmailDispatcher {
	return &EmailDispatcher{}
}

// Send records the message. It never fails.
func (d *EmailDispatcher) Send(_ context.Context, msg port.EmailMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, msg)
	return nil
}

// Sent returns a copy of every message dispatched so far.
func (d *EmailDispatcher) Sent() []port.EmailMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]port.EmailMessage, len(d.sent))
	copy(out, d.sent)
	return out
}

// SentCount returns how many messages have been dispatched.
func (d *EmailDispatcher) SentCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sent)
}

// Reset clears recorded messages, useful between sub-tests.
func (d *EmailDispatcher) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = nil
}
