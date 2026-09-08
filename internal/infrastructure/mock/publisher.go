// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"sync"
)

// PublishedMessage is one message recorded by Publisher.
type PublishedMessage struct {
	Subject string
	Payload []byte
}

// Publisher is an in-memory port.Publisher double. Every call is recorded so
// tests can assert on what was published without a NATS connection.
type Publisher struct {
	mu        sync.Mutex
	published []PublishedMessage
}

// NewPublisher constructs an empty double.
func NewPublisher() *Publisher {
	return &Publisher{}
}

// Publish records the message and always succeeds.
func (p *Publisher) Publish(_ context.Context, subject string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, PublishedMessage{Subject: subject, Payload: payload})
	return nil
}

// Published returns every message recorded so far.
func (p *Publisher) Published() []PublishedMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PublishedMessage, len(p.published))
	copy(out, p.published)
	return out
}
