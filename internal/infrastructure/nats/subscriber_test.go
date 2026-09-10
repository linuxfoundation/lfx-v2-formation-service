// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// publishTo sends one message from a connection the client under test does not
// own, standing in for the indexer.
func publishTo(t *testing.T, url, subject string, data []byte) {
	t.Helper()

	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("Connect() = %v, want no error", err)
	}
	defer conn.Close()

	if err := conn.Publish(subject, data); err != nil {
		t.Fatalf("Publish(%s) = %v, want no error", subject, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("Flush() = %v, want no error", err)
	}
}

func TestSubscribeDeliversToTheHandler(t *testing.T) {
	url := startTestNATSServer(t)
	subscriber := NewSubscriber(newTestClient(t, url, 2*time.Second))

	received := make(chan []byte, 1)
	stop, err := subscriber.Subscribe(context.Background(),
		ProjectUpdatedSubject, ProjectEventsQueue, func(data []byte) {
			received <- data
		})
	if err != nil {
		t.Fatalf("Subscribe() = %v, want no error", err)
	}
	defer stop()

	publishTo(t, url, ProjectUpdatedSubject, []byte(`{"action":"updated"}`))

	select {
	case got := <-received:
		if string(got) != `{"action":"updated"}` {
			t.Errorf("handler received %q, want the published body", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no message reached the handler")
	}
}

// The queue group is what keeps event-path work from multiplying by the replica
// count. Two subscriptions in one group are two replicas of this service, and
// one message must reach exactly one of them — if this ever regresses to a plain
// subscription, every replica does the work and nothing else looks wrong.
func TestOneQueueMemberHandlesEachMessage(t *testing.T) {
	url := startTestNATSServer(t)

	var mu sync.Mutex
	handled := 0
	done := make(chan struct{}, 2)

	for range 2 {
		subscriber := NewSubscriber(newTestClient(t, url, 2*time.Second))
		stop, err := subscriber.Subscribe(context.Background(),
			ProjectUpdatedSubject, ProjectEventsQueue, func(_ []byte) {
				mu.Lock()
				handled++
				mu.Unlock()
				done <- struct{}{}
			})
		if err != nil {
			t.Fatalf("Subscribe() = %v, want no error", err)
		}
		defer stop()
	}

	publishTo(t, url, ProjectUpdatedSubject, []byte(`{}`))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no replica handled the message")
	}

	// Long enough that a second delivery would have arrived. Nothing to wait
	// on for a message that must never come, so a bounded pause is the check.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if handled != 1 {
		t.Errorf("handled = %d, want exactly 1; the queue group is not sharing", handled)
	}
}

// A message from another service is data this service did not construct. A
// decode that panics on one malformed publish must not take down a replica that
// is otherwise serving reads.
func TestAPanickingHandlerDoesNotKillTheSubscription(t *testing.T) {
	url := startTestNATSServer(t)
	subscriber := NewSubscriber(newTestClient(t, url, 2*time.Second))

	survived := make(chan struct{}, 1)
	first := true
	stop, err := subscriber.Subscribe(context.Background(),
		ProjectUpdatedSubject, ProjectEventsQueue, func(_ []byte) {
			if first {
				first = false
				panic("malformed payload")
			}
			survived <- struct{}{}
		})
	if err != nil {
		t.Fatalf("Subscribe() = %v, want no error", err)
	}
	defer stop()

	publishTo(t, url, ProjectUpdatedSubject, []byte(`not json`))
	publishTo(t, url, ProjectUpdatedSubject, []byte(`{}`))

	select {
	case <-survived:
	case <-time.After(3 * time.Second):
		t.Fatal("the subscription did not survive a panicking handler")
	}
}

// Stopping waits for delivery to end rather than cutting it off, because a
// handler may be inside a database transaction when shutdown begins.
func TestStopEndsDelivery(t *testing.T) {
	url := startTestNATSServer(t)
	subscriber := NewSubscriber(newTestClient(t, url, 2*time.Second))

	var mu sync.Mutex
	handled := 0
	stop, err := subscriber.Subscribe(context.Background(),
		ProjectUpdatedSubject, ProjectEventsQueue, func(_ []byte) {
			mu.Lock()
			handled++
			mu.Unlock()
		})
	if err != nil {
		t.Fatalf("Subscribe() = %v, want no error", err)
	}

	stop()

	publishTo(t, url, ProjectUpdatedSubject, []byte(`{}`))
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if handled != 0 {
		t.Errorf("handled = %d after stop, want 0", handled)
	}
}
