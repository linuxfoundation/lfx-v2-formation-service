// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"sync"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// Subscriber stands in for the message transport, letting a test deliver a
// message directly to whatever subscribed.
//
// The alternative is an embedded NATS server, which the transport package's own
// tests do use — correctly, because what they are testing is the transport. A
// listener test is not: it is asking what the handler decides when given a
// payload, and routing that payload through a real broker adds a connection, a
// port and a timing window to a question that has none of those in it.
type Subscriber struct {
	mu       sync.Mutex
	handlers map[string]func(ctx context.Context, data []byte)
	queues   map[string]string
	ctxs     map[string]context.Context
	stopped  map[string]bool
	err      error
}

// Asserted here rather than left to the first test that uses it. Nothing
// consumes this fake until the listener lands, so without the assertion a
// signature that drifted from the port would compile cleanly and only fail
// later, in the change that is trying to use it.
var _ port.Subscriber = (*Subscriber)(nil)

// NewSubscriber constructs a subscriber with nothing subscribed.
func NewSubscriber() *Subscriber {
	return &Subscriber{
		handlers: map[string]func(ctx context.Context, data []byte){},
		queues:   map[string]string{},
		ctxs:     map[string]context.Context{},
		stopped:  map[string]bool{},
	}
}

// SetError makes every subsequent Subscribe fail, so a test can check that the
// service still starts when the broker is unreachable.
func (s *Subscriber) SetError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Subscribe records the handler and the queue it was registered under.
func (s *Subscriber) Subscribe(
	ctx context.Context, subject, queue string, handler func(ctx context.Context, data []byte),
) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return nil, s.err
	}
	s.handlers[subject] = handler
	s.queues[subject] = queue
	s.ctxs[subject] = ctx

	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.stopped[subject] = true
		delete(s.handlers, subject)
	}, nil
}

// Deliver hands data to the handler subscribed to subject, and reports whether
// anything was listening.
//
// Synchronous, so a test asserts on the result immediately after rather than
// polling for it. That does mean it cannot exercise concurrent delivery — a
// test that needs two handlers running at once calls this from two goroutines
// itself, which keeps the interleaving visible in the test rather than hidden
// in the fake.
func (s *Subscriber) Deliver(subject string, data []byte) bool {
	s.mu.Lock()
	handler := s.handlers[subject]
	ctx := s.ctxs[subject]
	s.mu.Unlock()

	if handler == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	handler(ctx, data)
	return true
}

// QueueFor returns the queue name a subject was subscribed under, or empty.
//
// Exposed because the queue name is load-bearing rather than cosmetic: it is
// what makes one replica of this service handle each message while other
// services still receive their own copy. A name that collided with another
// service's would silently take that service's messages, and nothing about the
// subscription would look wrong.
func (s *Subscriber) QueueFor(subject string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queues[subject]
}

// Subscribed reports whether a subject currently has a handler.
func (s *Subscriber) Subscribed(subject string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handlers[subject] != nil
}

// Stopped reports whether a subject's subscription was stopped.
func (s *Subscriber) Stopped(subject string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped[subject]
}
