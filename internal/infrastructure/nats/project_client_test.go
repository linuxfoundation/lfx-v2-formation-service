// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"errors"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
)

// startTestNATSServer runs an in-process NATS server, which is how both donor
// services test their NATS adapters: the code under test is the real client
// against a real connection, so a mistake in message construction or subject
// naming actually fails rather than being mocked away.
func startTestNATSServer(t *testing.T) string {
	t.Helper()

	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true}
	server, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer() = %v, want no error", err)
	}
	go server.Start()
	if !server.ReadyForConnections(4 * time.Second) {
		server.Shutdown()
		t.Fatal("NATS server not ready")
	}
	t.Cleanup(server.Shutdown)
	return server.ClientURL()
}

// respondOn subscribes a responder, standing in for the project service.
func respondOn(t *testing.T, url, subject string, reply func(request string) []byte) {
	t.Helper()

	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("Connect() = %v, want no error", err)
	}
	t.Cleanup(conn.Close)

	sub, err := conn.Subscribe(subject, func(msg *nats.Msg) {
		_ = msg.Respond(reply(string(msg.Data)))
	})
	if err != nil {
		t.Fatalf("Subscribe(%s) = %v, want no error", subject, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("Flush() = %v, want no error", err)
	}
}

func newTestClient(t *testing.T, url string, timeout time.Duration) *Client {
	t.Helper()

	client, err := New(context.Background(), Config{URL: url, Timeout: timeout})
	if err != nil {
		t.Fatalf("New() = %v, want no error", err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestProjectClientNameAndSlug(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	respondOn(t, url, ProjectGetNameSubject, func(uid string) []byte {
		if uid != "project-1" {
			return nil
		}
		return []byte("Example Project")
	})
	respondOn(t, url, ProjectGetSlugSubject, func(string) []byte { return []byte("example") })

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))

	name, err := p.Name(ctx, "project-1")
	if err != nil {
		t.Fatalf("Name() = %v, want no error", err)
	}
	if name != "Example Project" {
		t.Errorf("Name() = %q, want %q", name, "Example Project")
	}

	slug, err := p.Slug(ctx, "project-1")
	if err != nil {
		t.Fatalf("Slug() = %v, want no error", err)
	}
	if slug != "example" {
		t.Errorf("Slug() = %q, want %q", slug, "example")
	}
}

// An empty reply means the project has no such attribute, which is a not-found
// rather than an empty-string answer — otherwise a caller stores "" as a name.
func TestProjectClientEmptyReplyIsNotFound(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	respondOn(t, url, ProjectGetNameSubject, func(string) []byte { return []byte("") })

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))
	if _, err := p.Name(ctx, "project-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Name() = %v, want %v", err, domain.ErrNotFound)
	}
}

func TestProjectClientWriters(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	// Shaped like the real reply: the full user record per entry, of which only
	// the username is wanted.
	respondOn(t, url, ProjectGetWritersSubject, func(string) []byte {
		return []byte(`[{"name":"A Person","email":"a@example.org","username":"aperson","avatar":"x"},
		                {"name":"B Person","email":"b@example.org","username":"bperson","avatar":"y"}]`)
	})

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))
	writers, err := p.Writers(ctx, "project-1")
	if err != nil {
		t.Fatalf("Writers() = %v, want no error", err)
	}
	want := []string{"aperson", "bperson"}
	if len(writers) != len(want) {
		t.Fatalf("Writers() = %v, want %v", writers, want)
	}
	for i := range want {
		if writers[i] != want[i] {
			t.Errorf("Writers()[%d] = %q, want %q", i, writers[i], want[i])
		}
	}
}

// A project with no writers is a real state and must not read as a failure.
func TestProjectClientWritersEmptyArray(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	respondOn(t, url, ProjectGetWritersSubject, func(string) []byte { return []byte(`[]`) })

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))
	writers, err := p.Writers(ctx, "project-1")
	if err != nil {
		t.Fatalf("Writers() = %v, want no error", err)
	}
	if len(writers) != 0 {
		t.Errorf("Writers() = %v, want empty", writers)
	}
}

// An entry with no username is dropped rather than recorded as an empty
// assignee, which would match an unassigned row.
func TestProjectClientWritersSkipsBlankUsernames(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	respondOn(t, url, ProjectGetWritersSubject, func(string) []byte {
		return []byte(`[{"username":""},{"username":"aperson"}]`)
	})

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))
	writers, err := p.Writers(ctx, "project-1")
	if err != nil {
		t.Fatalf("Writers() = %v, want no error", err)
	}
	if len(writers) != 1 || writers[0] != "aperson" {
		t.Errorf("Writers() = %v, want [aperson]", writers)
	}
}

func TestProjectClientRejectsAnEmptyUID(t *testing.T) {
	ctx := context.Background()
	p := NewProjectClient(newTestClient(t, startTestNATSServer(t), 2*time.Second))

	if _, err := p.Name(ctx, ""); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Errorf("Name(\"\") = %v, want %v", err, domain.ErrInvalidRequest)
	}
	if _, err := p.Writers(ctx, ""); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Errorf("Writers(\"\") = %v, want %v", err, domain.ErrInvalidRequest)
	}
}

// With nobody answering, the call has to fail on the timeout rather than block
// indefinitely — these reads run inside an open transaction.
func TestRequestTimesOutWithNoResponder(t *testing.T) {
	ctx := context.Background()
	p := NewProjectClient(newTestClient(t, startTestNATSServer(t), 50*time.Millisecond))

	start := time.Now()
	_, err := p.Name(ctx, "project-1")
	if err == nil {
		t.Fatal("Name() = nil, want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v, want the configured timeout to bound it", elapsed)
	}
}

// The project service replies with zero bytes on every handler failure —
// unknown project, unparseable UID, store outage. That must not decode to an
// empty roster, which is the shape assignment validation refuses against.
func TestProjectClientWritersEmptyReplyIsNotFound(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	respondOn(t, url, ProjectGetWritersSubject, func(string) []byte { return nil })

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))
	if _, err := p.Writers(ctx, "project-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Writers() = %v, want %v", err, domain.ErrNotFound)
	}
}

// A malformed reply must be an error, not a silently empty roster: an empty
// roster is what assignment validation refuses against.
func TestProjectClientWritersRejectsMalformedJSON(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	respondOn(t, url, ProjectGetWritersSubject, func(string) []byte { return []byte(`not json`) })

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))
	if _, err := p.Writers(ctx, "project-1"); err == nil {
		t.Error("Writers() = nil, want a decode error")
	}
}

func TestClientIsReady(t *testing.T) {
	client := newTestClient(t, startTestNATSServer(t), 2*time.Second)
	if err := client.IsReady(); err != nil {
		t.Errorf("IsReady() = %v, want no error", err)
	}

	if err := (&Client{}).IsReady(); err == nil {
		t.Error("IsReady() on an unconnected client = nil, want an error")
	}
}
