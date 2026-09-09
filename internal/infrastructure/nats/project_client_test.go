// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
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
// indefinitely: a caller waiting on an unanswered request would otherwise hold
// whatever it is holding for as long as the project service is unreachable.
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

// The read assignment validation depends on: both halves of the roster from one
// record, so an auditor is not refused for being absent from the writers list.
func TestProjectClientGetSettings(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	var gotRequest string
	respondOn(t, url, ProjectGetSettingsSubject, func(request string) []byte {
		gotRequest = request
		return []byte(`{"uid":"project-1",
		                "announcement_date":"2026-06-17T00:00:00Z",
		                "writers":[{"username":"awriter","email":"a@example.org"}],
		                "auditors":[{"username":"anauditor"},{"username":""}]}`)
	})

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))
	settings, err := p.GetSettings(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetSettings() = %v, want no error", err)
	}

	// The subject takes the bare UID, not a JSON envelope.
	if gotRequest != "project-1" {
		t.Errorf("request body = %q, want %q", gotRequest, "project-1")
	}
	if len(settings.Writers) != 1 || settings.Writers[0] != "awriter" {
		t.Errorf("writers = %v, want [awriter]", settings.Writers)
	}
	// The blank username is dropped rather than carried: an empty string would
	// match an unassigned item.
	if len(settings.Auditors) != 1 || settings.Auditors[0] != "anauditor" {
		t.Errorf("auditors = %v, want [anauditor]", settings.Auditors)
	}
	// Narrowed to a date, which is the precision due-date arithmetic uses.
	if settings.AnnouncementDate == nil || *settings.AnnouncementDate != "2026-06-17" {
		t.Errorf("announcement_date = %v, want 2026-06-17", settings.AnnouncementDate)
	}
}

// An unconfigured roster is a real state. It must arrive as an empty roster with
// no announcement date, not as an error and not as a nil dereference.
func TestProjectClientGetSettingsWithNothingConfigured(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	respondOn(t, url, ProjectGetSettingsSubject, func(string) []byte {
		return []byte(`{"uid":"project-1","announcement_date":null,"writers":[],"auditors":[]}`)
	})

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))
	settings, err := p.GetSettings(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetSettings() = %v, want no error", err)
	}
	if len(settings.Writers) != 0 || len(settings.Auditors) != 0 {
		t.Errorf("roster = %v/%v, want both empty", settings.Writers, settings.Auditors)
	}
	if settings.AnnouncementDate != nil {
		t.Errorf("announcement_date = %v, want nil", *settings.AnnouncementDate)
	}
}

// A project with no settings record must be not-found rather than an empty
// roster: assignment validation refuses against an empty roster, so conflating
// the two would reject every legitimate assignee on an upstream gap.
func TestProjectClientGetSettingsEmptyReplyIsNotFound(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	respondOn(t, url, ProjectGetSettingsSubject, func(string) []byte { return nil })

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))
	_, err := p.GetSettings(ctx, "project-1")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetSettings() = %v, want domain.ErrNotFound", err)
	}
}

// The sweep's list, and the assertion that matters most is on the request: the
// stages it watches plus the projects it already holds a checklist for. Without
// the second half a checklist whose project has gone Active is invisible to the
// sweep, and the lifecycle that completes it never runs.
func TestProjectClientListFormingProjects(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	var gotRequest string
	respondOn(t, url, ProjectListProjectsSubject, func(request string) []byte {
		gotRequest = request
		return []byte(`[{"uid":"p1","slug":"one","is_foundation":true,"parent_uid":"","stage":"Formation - Engaged"},
		                {"uid":"p2","slug":"two","is_foundation":false,"parent_uid":"p1","stage":"Active"},
		                {"uid":"","slug":"nameless","stage":"Active"}]`)
	})

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))
	refs, err := p.ListFormingProjects(ctx, []string{"p2"})
	if err != nil {
		t.Fatalf("ListFormingProjects() = %v, want no error", err)
	}

	var request projectListRequest
	if err := json.Unmarshal([]byte(gotRequest), &request); err != nil {
		t.Fatalf("request %q is not the JSON body this subject takes: %v", gotRequest, err)
	}
	if len(request.Stages) != len(model.FormationStages()) {
		t.Errorf("stages = %v, want all of %v", request.Stages, model.FormationStages())
	}
	// Disengaged is asked for even though it creates nothing, because a project
	// that has just become Disengaged has a checklist to freeze.
	var askedForDisengaged bool
	for _, stage := range request.Stages {
		if stage == model.StageFormationDisengaged {
			askedForDisengaged = true
		}
	}
	if !askedForDisengaged {
		t.Error("the request omits the Disengaged stage, whose checklists need freezing")
	}
	if len(request.UIDs) != 1 || request.UIDs[0] != "p2" {
		t.Errorf("uids = %v, want [p2]", request.UIDs)
	}

	// The blank-UID entry is dropped: it cannot be swept, and passing it on
	// would have the sweep count it.
	if len(refs) != 2 {
		t.Fatalf("refs = %d, want 2 (the entry naming no project is dropped)", len(refs))
	}
	// Stage lands on SubStage — the same compound value under the older name.
	if refs[0].SubStage != model.StageFormationEngaged || !refs[0].IsFoundation || refs[0].Slug != "one" {
		t.Errorf("refs[0] = %+v, want the Engaged foundation with slug one", refs[0])
	}
	// The whole point of the uids half: a project no longer in formation comes
	// back, carrying the stage that tells the sweep to complete its checklist.
	if refs[1].SubStage != model.StageActive || refs[1].ParentUID != "p1" {
		t.Errorf("refs[1] = %+v, want the Active child of p1", refs[1])
	}
}

// No project holds a checklist yet, so there is nothing to name. The stages must
// still be asked for, and the uids key must be absent rather than an empty array.
func TestProjectClientListFormingProjectsWithNoKnownChecklists(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	var gotRequest string
	respondOn(t, url, ProjectListProjectsSubject, func(request string) []byte {
		gotRequest = request
		return []byte(`[]`)
	})

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))
	refs, err := p.ListFormingProjects(ctx, nil)
	if err != nil {
		t.Fatalf("ListFormingProjects() = %v, want no error", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %v, want none", refs)
	}
	// omitempty, so the subject sees a stages-only request rather than one
	// carrying an empty filter it has to interpret.
	if strings.Contains(gotRequest, `"uids"`) {
		t.Errorf("request = %s, want no uids key when nothing holds a checklist", gotRequest)
	}
}

// An empty reply is an upstream failure, and here it must not read as "nothing
// is being formed": the sweep would report a successful tick having created
// nothing, which is indistinguishable from working correctly.
func TestProjectClientListFormingProjectsEmptyReplyIsNotFound(t *testing.T) {
	ctx := context.Background()
	url := startTestNATSServer(t)
	respondOn(t, url, ProjectListProjectsSubject, func(string) []byte { return nil })

	p := NewProjectClient(newTestClient(t, url, 2*time.Second))
	_, err := p.ListFormingProjects(ctx, nil)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("ListFormingProjects() = %v, want domain.ErrNotFound", err)
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
