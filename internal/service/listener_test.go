// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
	infranats "github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/nats"
)

// The real decoder, not a stand-in.
//
// These tests are about what the listener decides given what actually arrives on
// the subject, and a hand-written fake decoder would let the pair pass while
// disagreeing with the wire. The payloads below are the shape observed in the
// dev index: uid, slug, stage and parent_uid under data, parent_refs alongside
// it, is_foundation absent when false.
func decodeForTest() func([]byte) (port.ProjectRef, error) {
	return infranats.DecodeProjectEvent
}

// projectEventJSON builds the published event for a project at a stage.
//
// An empty stage omits the field entirely rather than sending "", because that
// is what the index holds: most project documents carry no stage at all, and a
// decoder tested only against an explicit empty string would not be tested
// against the common case.
func projectEventJSON(t *testing.T, uid, slug, stage string) []byte {
	t.Helper()

	data := map[string]any{"uid": uid, "slug": slug, "parent_uid": "parent-1"}
	if stage != "" {
		data["stage"] = stage
	}

	payload, err := json.Marshal(map[string]any{
		"object_id":   uid,
		"object_type": "project",
		"action":      "updated",
		"body": map[string]any{
			"object_type": "project",
			"object_id":   uid,
			"parent_refs": []string{"project:parent-1"},
			"data":        data,
		},
	})
	if err != nil {
		t.Fatalf("building the event: %v", err)
	}
	return payload
}

// newListener wires a listener over the shared expansion fixture.
func newListener(t *testing.T, projects port.ProjectReader) (*ProjectListener, *expansionFixture) {
	t.Helper()
	r, f := newReconciler(t, projects)
	return NewProjectListener(r, decodeForTest()), f
}

// An event whose document carries no stage must change nothing.
//
// This is the common case rather than a curiosity: in dev only 167 of 1032
// indexed project documents carry a stage at all. The tempting reading of a
// missing stage is "not forming", and it is the dangerous one — it would freeze
// live checklists for the great majority of projects on the strength of a field
// that was simply never published. Leave it to the sweep, which asks the project
// service rather than reading an indexed copy.
func TestAnEventWithNoStageChangesNothing(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{}
	listener, f := newListener(t, projects)

	listener.Handle(ctx, projectEventJSON(t, "project-1", "p1", ""))

	if _, err := f.formations.GetByProject(ctx, "project-1"); err == nil {
		t.Error("a checklist was created for a project whose stage is unknown")
	}
	counts := listener.Counts()
	if counts.Received != 1 {
		t.Errorf("received = %d, want 1", counts.Received)
	}
	if counts.DroppedStage != 1 {
		t.Errorf("dropped for no stage = %d, want 1", counts.DroppedStage)
	}
	if counts.Handled != 0 {
		t.Errorf("handled = %d, want 0 — nothing was reconciled", counts.Handled)
	}
}

// A Confidential project gets a checklist from the event, like any other
// forming stage.
//
// Worth its own test because Confidential is the stage most likely to be
// special-cased by mistake: it is the one whose projects are hidden from
// ordinary users, and hiding a project from readers is not the same as
// declining to prepare its checklist.
func TestAConfidentialProjectGetsAChecklistFromAnEvent(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{}
	listener, f := newListener(t, projects)

	listener.Handle(ctx, projectEventJSON(t, "secret-1", "secret", model.StageFormationConfidential))

	formation, err := f.formations.GetByProject(ctx, "secret-1")
	if err != nil {
		t.Fatalf("no checklist for the confidential project: %v", err)
	}
	items, err := f.items.ListByFormation(ctx, formation.UID)
	if err != nil {
		t.Fatalf("listing items: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("items = %d, want 3", len(items))
	}
	if counts := listener.Counts(); counts.Handled != 1 || counts.Failed != 0 {
		t.Errorf("counts = %+v, want one handled and none failed", counts)
	}
}

// The event path must reach the same decision as the sweep, because the listener
// holds no reconcile logic of its own.
//
// Checked by outcome rather than by asserting a call: two reconcilers over
// identical fixtures, one driven by an event and one by a sweep, must end in the
// same state. That is the property that matters — a listener that agreed with
// the sweep only by having copied its rules would pass a call-count assertion
// and fail this one the day the rules changed.
func TestTheEventPathAndTheSweepAgree(t *testing.T) {
	ctx := context.Background()
	ref := port.ProjectRef{UID: "project-1", Slug: "p1", ParentUID: "parent-1", SubStage: model.StageFormationEngaged}

	// Driven by an event.
	listener, viaEvent := newListener(t, &listProjects{})
	listener.Handle(ctx, projectEventJSON(t, ref.UID, ref.Slug, ref.SubStage))

	// Driven by a sweep over the same project.
	sweeper, viaSweep := newReconciler(t, &listProjects{refs: []port.ProjectRef{ref}})
	if _, err := sweeper.ReconcileOnce(ctx); err != nil {
		t.Fatalf("sweep = %v, want no error", err)
	}

	eventFormation, err := viaEvent.formations.GetByProject(ctx, ref.UID)
	if err != nil {
		t.Fatalf("the event created no checklist: %v", err)
	}
	sweepFormation, err := viaSweep.formations.GetByProject(ctx, ref.UID)
	if err != nil {
		t.Fatalf("the sweep created no checklist: %v", err)
	}

	if eventFormation.Lifecycle != sweepFormation.Lifecycle {
		t.Errorf("lifecycle: event = %q, sweep = %q", eventFormation.Lifecycle, sweepFormation.Lifecycle)
	}
	if eventFormation.ProjectUID != sweepFormation.ProjectUID {
		t.Errorf("project: event = %q, sweep = %q", eventFormation.ProjectUID, sweepFormation.ProjectUID)
	}

	eventItems, err := viaEvent.items.ListByFormation(ctx, eventFormation.UID)
	if err != nil {
		t.Fatalf("listing the event's items: %v", err)
	}
	sweepItems, err := viaSweep.items.ListByFormation(ctx, sweepFormation.UID)
	if err != nil {
		t.Fatalf("listing the sweep's items: %v", err)
	}
	if len(eventItems) != len(sweepItems) {
		t.Errorf("items: event = %d, sweep = %d", len(eventItems), len(sweepItems))
	}
}

// A payload the listener cannot read is dropped, and the next one is still
// handled.
//
// The transport offers no redelivery, so dropping is the whole of the failure
// path — there is nothing to retry and nobody to return an error to. What must
// not happen is the bad message taking the subscription down with it, which
// would turn one malformed event into an indefinitely dead accelerator.
func TestABadPayloadIsDroppedAndTheNextOneIsHandled(t *testing.T) {
	ctx := context.Background()
	listener, f := newListener(t, &listProjects{})

	for _, bad := range [][]byte{
		[]byte("not json at all"),
		[]byte(`{"object_type":"committee","body":{"data":{"uid":"c1"}}}`),
		[]byte(`{"object_id":"p1","object_type":"project"}`),
		[]byte(`{"object_type":"project","body":{"data":{"slug":"no-uid"}}}`),
	} {
		listener.Handle(ctx, bad)
	}

	listener.Handle(ctx, projectEventJSON(t, "good-1", "good", model.StageFormationEngaged))

	if _, err := f.formations.GetByProject(ctx, "good-1"); err != nil {
		t.Fatalf("the event after the bad ones was not handled: %v", err)
	}
	counts := listener.Counts()
	if counts.DroppedOther != 4 {
		t.Errorf("dropped as unreadable = %d, want 4", counts.DroppedOther)
	}
	if counts.Handled != 1 {
		t.Errorf("handled = %d, want 1", counts.Handled)
	}
}

// The same event arriving twice must leave one checklist.
//
// Not hypothetical: the transport is at-most-once per queue group, but the
// project service publishes an update for changes that are not stage changes,
// so the listener sees the same project repeatedly. Idempotence here is the
// existing uniqueness constraint doing its job, and this pins that the event
// path goes through it rather than around it.
func TestARepeatedEventCreatesOneChecklist(t *testing.T) {
	ctx := context.Background()
	listener, f := newListener(t, &listProjects{})

	event := projectEventJSON(t, "project-1", "p1", model.StageFormationEngaged)
	for range 4 {
		listener.Handle(ctx, event)
	}

	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	items, err := f.items.ListByFormation(ctx, formation.UID)
	if err != nil {
		t.Fatalf("ListByFormation() = %v, want no error", err)
	}
	if len(items) != 3 {
		t.Errorf("items = %d, want 3 — the repeats added their own", len(items))
	}
	if counts := listener.Counts(); counts.Handled != 4 || counts.Failed != 0 {
		t.Errorf("counts = %+v, want four handled and none failed", counts)
	}
}

// An event and a sweep racing over the same project must still leave one
// checklist.
//
// This is the ordinary state of a deployed pod rather than a contrived one: the
// sweep runs on every replica with no leader election, and the listener runs
// alongside it. With the sweep now daily the window is wider than it looks —
// a sweep takes long enough to walk every forming project, and an event can land
// anywhere inside it.
func TestAnEventRacingASweepCreatesOneChecklist(t *testing.T) {
	ctx := context.Background()
	ref := port.ProjectRef{UID: "project-1", Slug: "p1", SubStage: model.StageFormationEngaged}
	projects := &listProjects{refs: []port.ProjectRef{ref}}

	reconciler, f := newReconciler(t, projects)
	listener := NewProjectListener(reconciler, decodeForTest())
	event := projectEventJSON(t, ref.UID, ref.Slug, ref.SubStage)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			listener.Handle(ctx, event)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reconciler.ReconcileOnce(ctx); err != nil {
				t.Errorf("ReconcileOnce() = %v, want no error", err)
			}
		}()
	}
	wg.Wait()

	formation, err := f.formations.GetByProject(ctx, ref.UID)
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	items, err := f.items.ListByFormation(ctx, formation.UID)
	if err != nil {
		t.Fatalf("ListByFormation() = %v, want no error", err)
	}
	if len(items) != 3 {
		t.Errorf("items = %d, want 3", len(items))
	}
}

// A reconcile that fails is counted and does not propagate.
//
// The listener cannot report a failure to anyone — there is no acknowledgement
// on this transport — so the only useful thing it can do is count it, and the
// count is what tells an operator the accelerator is running but not working.
// Provoked with a blank UID, which the expander refuses, the same way the sweep
// test does.
//
// The one test here that substitutes the decoder, because it has to: the real
// one refuses a blank UID before the reconcile is ever reached, which is the
// behaviour the bad-payload test above pins. Reaching past it is the only way to
// ask what happens when the reconcile itself fails, and that question is worth
// asking separately — the two failures are counted differently and mean
// different things to whoever reads the counts.
func TestAFailedReconcileIsCountedRatherThanRaised(t *testing.T) {
	ctx := context.Background()
	reconciler, _ := newReconciler(t, &listProjects{})

	decode := func(data []byte) (port.ProjectRef, error) {
		if string(data) == "blank-uid" {
			return port.ProjectRef{SubStage: model.StageFormationEngaged}, nil
		}
		return infranats.DecodeProjectEvent(data)
	}
	listener := NewProjectListener(reconciler, decode)

	listener.Handle(ctx, []byte("blank-uid"))
	listener.Handle(ctx, projectEventJSON(t, "good-1", "good", model.StageFormationEngaged))

	counts := listener.Counts()
	if counts.Failed != 1 {
		t.Errorf("failed = %d, want 1", counts.Failed)
	}
	if counts.Handled != 2 {
		t.Errorf("handled = %d, want 2 — a failure is still an event handled", counts.Handled)
	}
}

// Which path created a checklist has to survive in the record, not only in the
// logs.
//
// With the sweep daily and the listener in front of it, "is the accelerator
// working" is a question asked about checklists that already exist, often days
// later. Logs are gone by then. This also pins that the distinction stays inside
// the system actor: SetBy and Actor must read exactly as they did before, or
// every consumer of the feed has quietly had its meaning changed.
func TestTheCreatingPathIsRecordedWithoutChangingTheActor(t *testing.T) {
	ctx := context.Background()
	listener, f := newListener(t, &listProjects{})

	listener.Handle(ctx, projectEventJSON(t, "project-1", "p1", model.StageFormationEngaged))

	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	entries, _, err := f.activity.List(ctx, formation.UID, "", 50)
	if err != nil {
		t.Fatalf("List() = %v, want no error", err)
	}

	var found bool
	for _, entry := range entries {
		if entry.Action != ActionTemplateExpanded {
			continue
		}
		found = true
		if got := entry.After["trigger"]; got != string(TriggerListener) {
			t.Errorf("trigger = %v, want %q", got, TriggerListener)
		}
		if entry.Actor != actorSystem {
			t.Errorf("actor = %q, want %q — the trigger must not displace it", entry.Actor, actorSystem)
		}
		if entry.SetBy != model.SetBySystem {
			t.Errorf("set_by = %q, want %q", entry.SetBy, model.SetBySystem)
		}
	}
	if !found {
		t.Fatal("the expansion recorded no activity entry")
	}
}

// The sweep records itself as the sweep, so the two paths are told apart rather
// than both reading as "the system".
func TestTheSweepRecordsItselfAsTheTrigger(t *testing.T) {
	ctx := context.Background()
	projects := &listProjects{refs: []port.ProjectRef{
		{UID: "project-1", SubStage: model.StageFormationEngaged},
	}}
	r, f := newReconciler(t, projects)

	if _, err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce() = %v, want no error", err)
	}

	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	entries, _, err := f.activity.List(ctx, formation.UID, "", 50)
	if err != nil {
		t.Fatalf("List() = %v, want no error", err)
	}
	for _, entry := range entries {
		if entry.Action == ActionTemplateExpanded {
			if got := entry.After["trigger"]; got != string(TriggerSweep) {
				t.Errorf("trigger = %v, want %q", got, TriggerSweep)
			}
		}
	}
}

// Start attaches to every subject under one queue, so one replica handles each
// message.
func TestStartSubscribesEverySubjectUnderTheQueue(t *testing.T) {
	ctx := context.Background()
	listener, f := newListener(t, &listProjects{})
	subscriber := mock.NewSubscriber()

	stop, err := listener.Start(ctx, subscriber, infranats.ProjectEventsQueue,
		infranats.ProjectCreatedSubject, infranats.ProjectUpdatedSubject)
	if err != nil {
		t.Fatalf("Start() = %v, want no error", err)
	}

	for _, subject := range []string{infranats.ProjectCreatedSubject, infranats.ProjectUpdatedSubject} {
		if !subscriber.Subscribed(subject) {
			t.Errorf("%s has no handler", subject)
		}
		if got := subscriber.QueueFor(subject); got != infranats.ProjectEventsQueue {
			t.Errorf("%s queue = %q, want %q", subject, got, infranats.ProjectEventsQueue)
		}
	}

	// The handler that was registered is the one that reconciles.
	if !subscriber.Deliver(infranats.ProjectCreatedSubject,
		projectEventJSON(t, "project-1", "p1", model.StageFormationEngaged)) {
		t.Fatal("nothing was listening on the created subject")
	}
	if _, err := f.formations.GetByProject(ctx, "project-1"); err != nil {
		t.Errorf("the delivered event created no checklist: %v", err)
	}

	stop()
	for _, subject := range []string{infranats.ProjectCreatedSubject, infranats.ProjectUpdatedSubject} {
		if !subscriber.Stopped(subject) {
			t.Errorf("%s was not stopped", subject)
		}
	}
}

// Cancelling the service's context must not cancel a handler that is still
// running.
//
// Shutdown cancels the context first and drains afterwards. A handler that
// inherited that context would be told to give up at the moment the drain began
// waiting for it to finish, abandoning a database transaction halfway — so the
// drain would be doing nothing except delaying the abort. The handler's context
// is only cancelled once the drain has returned.
func TestShutdownDoesNotCancelAHandlerBeforeItIsDrained(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	listener, f := newListener(t, &listProjects{})
	subscriber := mock.NewSubscriber()

	stop, err := listener.Start(ctx, subscriber, infranats.ProjectEventsQueue, infranats.ProjectUpdatedSubject)
	if err != nil {
		t.Fatalf("Start() = %v, want no error", err)
	}

	// Shutdown begins: the context is cancelled before anything is drained.
	cancel()

	if !subscriber.Deliver(infranats.ProjectUpdatedSubject,
		projectEventJSON(t, "project-1", "p1", model.StageFormationEngaged)) {
		t.Fatal("nothing was listening")
	}
	if _, err := f.formations.GetByProject(context.Background(), "project-1"); err != nil {
		t.Fatalf("the in-flight handler was cancelled by shutdown: %v", err)
	}

	stop()
}

// A subscription that cannot be established unwinds the ones already made.
//
// A half-attached listener is the worst of the three outcomes: it accelerates
// creations but not updates, or the reverse, and the asymmetry is invisible
// until someone notices one kind of change is slow.
func TestAFailedSubscribeLeavesNothingAttached(t *testing.T) {
	ctx := context.Background()
	listener, _ := newListener(t, &listProjects{})
	subscriber := mock.NewSubscriber()
	subscriber.SetError(errors.New("broker unreachable"))

	if _, err := listener.Start(ctx, subscriber, infranats.ProjectEventsQueue,
		infranats.ProjectCreatedSubject, infranats.ProjectUpdatedSubject); err == nil {
		t.Fatal("Start() = nil, want the subscribe error")
	}
	if subscriber.Subscribed(infranats.ProjectCreatedSubject) {
		t.Error("a handler was left attached after Start failed")
	}
}
