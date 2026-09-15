// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// The chain is what lets a foundation's queue resolve at any depth instead of
// one level. Several of these encode a failure posture that reads like an
// inefficiency and is not — the write path declining to publish, in
// particular, is the one that looks most like a bug and is the whole point.

// tree seeds a reader with a parent chain, root last. The root is given an
// empty parent, which is how the platform root actually answers.
func tree(uids ...string) *mock.ProjectReader {
	projects := mock.NewProjectReader()
	refs := make([]port.ProjectRef, 0, len(uids))
	for i, uid := range uids {
		parent := ""
		if i+1 < len(uids) {
			parent = uids[i+1]
		}
		refs = append(refs, port.ProjectRef{UID: uid, Slug: uid, ParentUID: parent})
	}
	projects.SetProjectsByUID(refs)
	return projects
}

// refOf returns the seeded ref for a UID, so a test can hand Refresh the same
// project the reader knows about rather than a hand-built one that disagrees.
func refOf(t *testing.T, projects *mock.ProjectReader, uid string) port.ProjectRef {
	t.Helper()
	ref, err := projects.GetRef(context.Background(), uid)
	if err != nil {
		t.Fatalf("seeding: GetRef(%q) = %v", uid, err)
	}
	return ref
}

func TestChainResolvesEveryGenerationNearestFirst(t *testing.T) {
	projects := tree("project-1", "intermediate-1", "foundation-1", "root-1")
	projector := NewProjector(nil, nil, projects, nil)

	chain, outcome := projector.ancestorChain(context.Background(), refOf(t, projects, "project-1"))

	if outcome != chainComplete {
		t.Error("outcome is not chainComplete for a chain whose every generation is readable")
	}
	want := []string{"project-1", "intermediate-1", "foundation-1", "root-1"}
	if len(chain) != len(want) {
		t.Fatalf("chain = %v, want %v", chain, want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Fatalf("chain = %v, want %v — order is load-bearing, nearest first", chain, want)
		}
	}
}

// Scoping to a foundation has to return that foundation's own row as well as
// everything beneath it, which is only true if the project is in its own
// chain.
func TestChainStartsWithTheProjectItself(t *testing.T) {
	projects := tree("project-1", "foundation-1", "root-1")
	projector := NewProjector(nil, nil, projects, nil)

	chain, _ := projector.ancestorChain(context.Background(), refOf(t, projects, "project-1"))

	if len(chain) == 0 || chain[0] != "project-1" {
		t.Errorf("chain = %v, want the project's own UID first", chain)
	}
}

// A project sitting directly under the platform root is two entries, not one
// and not three. This is the shape most top-level projects have in production.
func TestAProjectDirectlyUnderTheRootIsATwoEntryChain(t *testing.T) {
	projects := tree("foundation-1", "root-1")
	projector := NewProjector(nil, nil, projects, nil)

	chain, outcome := projector.ancestorChain(context.Background(), refOf(t, projects, "foundation-1"))

	if outcome != chainComplete {
		t.Error("outcome is not chainComplete — an empty parent is the ordinary way out")
	}
	if len(chain) != 2 || chain[0] != "foundation-1" || chain[1] != "root-1" {
		t.Errorf("chain = %v, want [foundation-1 root-1]", chain)
	}
}

// The root terminates the walk by having no parent, so nothing special-cases
// it — and it is in the chain, which is what makes scoping to the root an
// ordinary query returning everything rather than a filter that gets skipped.
func TestTheChainReachesTheRootAndIncludesIt(t *testing.T) {
	projects := tree("project-1", "foundation-1", "root-1")
	projector := NewProjector(nil, nil, projects, nil)

	chain, outcome := projector.ancestorChain(context.Background(), refOf(t, projects, "project-1"))

	if outcome != chainComplete {
		t.Fatalf("outcome is not chainComplete; chain = %v", chain)
	}
	if chain[len(chain)-1] != "root-1" {
		t.Errorf("chain = %v, want it to end at the root", chain)
	}
}

// A cycle is data corruption rather than a shape the hierarchy can legally
// take, so the guarantee is termination, not a correct answer.
func TestACycleTerminatesAsAPartialChain(t *testing.T) {
	projects := mock.NewProjectReader()
	projects.SetProjectsByUID([]port.ProjectRef{
		{UID: "a", ParentUID: "b"},
		{UID: "b", ParentUID: "c"},
		{UID: "c", ParentUID: "a"},
	})
	projector := NewProjector(nil, nil, projects, nil)

	done := make(chan struct{})
	var chain []string
	var outcome chainOutcome
	go func() {
		chain, outcome = projector.ancestorChain(context.Background(), refOf(t, projects, "a"))
		close(done)
	}()
	<-done

	if outcome != chainFinal {
		t.Error("a cycle is structural, so the walk is final rather than worth retrying")
	}
	if len(chain) > maxAncestorDepth+1 {
		t.Errorf("chain = %v, longer than the cap allows", chain)
	}
}

// Both bounds report the same way. A reader of the counters should not need to
// know which one was hit to know the row is scoped to less than its parentage.
func TestAChainLongerThanTheCapStopsAtTheCap(t *testing.T) {
	uids := make([]string, 0, maxAncestorDepth+5)
	for i := 0; i < maxAncestorDepth+5; i++ {
		uids = append(uids, string(rune('a'+i))+"-project")
	}
	projects := tree(uids...)
	projector := NewProjector(nil, nil, projects, nil)

	chain, outcome := projector.ancestorChain(context.Background(), refOf(t, projects, uids[0]))

	if outcome != chainFinal {
		t.Error("the depth cap is structural, so the walk is final rather than worth retrying")
	}
	if len(chain) != maxAncestorDepth+1 {
		t.Errorf("chain length = %d, want %d — the project plus one per generation up to the cap",
			len(chain), maxAncestorDepth+1)
	}
}

// An ancestor that cannot be read stops the walk but does not discard what was
// already established: the row is scoped to the foundations it is known to sit
// under, rather than to none of them.
//
// A deleted ancestor is final rather than retryable. The project service
// answered — it said there is no such project — and it will answer the same way
// on every later pass, so holding the row back would hold it back for good.
func TestADeletedAncestorLeavesTheResolvedPrefix(t *testing.T) {
	projects := mock.NewProjectReader()
	projects.SetProjectsByUID([]port.ProjectRef{
		{UID: "project-1", ParentUID: "intermediate-1"},
		{UID: "intermediate-1", ParentUID: "missing-1"},
		// missing-1 is deliberately unseeded: the reader answers not-found.
	})
	projector := NewProjector(nil, nil, projects, nil)

	chain, outcome := projector.ancestorChain(context.Background(), refOf(t, projects, "project-1"))

	if outcome != chainFinal {
		t.Error("a deleted ancestor reads the same way every time, so the walk is final")
	}
	want := []string{"project-1", "intermediate-1", "missing-1"}
	if len(chain) != len(want) {
		t.Fatalf("chain = %v, want %v — the unreadable UID is itself a known ancestor", chain, want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Fatalf("chain = %v, want %v", chain, want)
		}
	}
}

// Its counterpart: the project service could not answer at all, which may go
// away on its own. That is the one shortfall worth holding a republish for.
//
// The distinction is the project client's, not this walk's — a reply naming no
// project is ErrNotFound, while an unreachable service or a failed handler
// arrives as an ordinary error. Collapsing the two here would freeze every row
// beneath a deleted project.
func TestAnUnreachableProjectServiceLeavesTheWalkRetryable(t *testing.T) {
	projects := mock.NewProjectReader()
	projects.SetProjectsByUID([]port.ProjectRef{
		{UID: "project-1", ParentUID: "intermediate-1"},
		{UID: "intermediate-1", ParentUID: "foundation-1"},
		{UID: "foundation-1"},
	})
	projects.SetRefErrorFor("foundation-1", errors.New("no responders available"))
	projector := NewProjector(nil, nil, projects, nil)

	chain, outcome := projector.ancestorChain(context.Background(), refOf(t, projects, "project-1"))

	if outcome != chainRetryable {
		t.Error("an unreachable service may answer next time, so the walk is retryable")
	}
	if !chainIs(chain, "project-1", "intermediate-1", "foundation-1") {
		t.Errorf("chain = %v, want the resolved prefix", chain)
	}
}

// The scheduled path publishes what resolved. It cannot skip: a row being
// published for the first time has no earlier document to fall back on, so
// skipping would leave it absent everywhere rather than present in fewer
// places.
func TestTheScheduledPathPublishesAPartialChainAndCountsIt(t *testing.T) {
	ctx := context.Background()
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	publisher := mock.NewIndexerPublisher()
	projects := mock.NewProjectReader()
	projects.SetProjectsByUID([]port.ProjectRef{
		{UID: "project-1", ParentUID: "missing-1"},
	})
	projector := NewProjector(formations, items, projects, publisher)

	if _, err := formations.Create(ctx, &model.Formation{ProjectUID: "project-1"}); err != nil {
		t.Fatalf("seeding formation = %v", err)
	}

	published, err := projector.Refresh(ctx, refOf(t, projects, "project-1"), FirstPublish)
	if err != nil {
		t.Fatalf("Refresh() = %v, want no error — a partial chain is not a failure here", err)
	}
	if !published {
		t.Fatal("published = false, want true — the scheduled path publishes what it resolved")
	}
	if got := projector.PartialChains(); got != 1 {
		t.Errorf("PartialChains() = %d, want 1 — a shortfall has to be visible without reading the index", got)
	}

	doc := publisher.Latest("project-1")
	if doc == nil {
		t.Fatal("no document published for project-1")
	}
	if len(doc.AncestorUIDs) == 0 || doc.AncestorUIDs[0] != "project-1" {
		t.Errorf("ancestor_uids = %v, want the resolved prefix", doc.AncestorUIDs)
	}
}

// The write path does the opposite, and this is the test most likely to be
// "fixed" by someone reading it as a missing publish. Republishing a shorter
// chain would drop the row out of its foundation's queue until the next sweep
// — reintroducing, as a side effect of the fix, the silent under-report the
// fix exists to remove.
func TestTheWritePathLeavesTheExistingRowRatherThanShorteningIt(t *testing.T) {
	ctx := context.Background()
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	publisher := mock.NewIndexerPublisher()
	projects := mock.NewProjectReader()
	projects.SetProjectsByUID([]port.ProjectRef{
		{UID: "project-1", ParentUID: "unreachable-1"},
	})
	// Unreachable rather than absent: only a shortfall a later pass could
	// resolve is worth withholding a row over.
	projects.SetRefErrorFor("unreachable-1", errors.New("no responders available"))
	projector := NewProjector(formations, items, projects, publisher)

	if _, err := formations.Create(ctx, &model.Formation{ProjectUID: "project-1"}); err != nil {
		t.Fatalf("seeding formation = %v", err)
	}

	published, err := projector.Refresh(ctx, refOf(t, projects, "project-1"), Republish)
	if err == nil {
		t.Fatal("Refresh() = nil error, want one — a withheld row must reach the refresher's " +
			"failure count rather than its nothing-to-publish count")
	}
	if published {
		t.Error("published = true, want false — the document already indexed is more complete")
	}
	if got := publisher.Count(); got != 0 {
		t.Errorf("published %d checklist documents, want 0 — a shorter chain must not overwrite a longer one", got)
	}
	if got := projector.PartialChains(); got != 1 {
		t.Errorf("PartialChains() = %d, want 1 — withheld still counts as a shortfall", got)
	}
}

// Withholding the checklist row must not withhold the item rows with it. Item
// documents carry no ancestry, so nothing about them is uncertain here — and
// stalling them until the next sweep would defeat the only reason the
// write-triggered path exists.
func TestWithholdingTheRowStillPublishesTheItemRows(t *testing.T) {
	ctx := context.Background()
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	publisher := mock.NewIndexerPublisher()
	projects := mock.NewProjectReader()
	projects.SetProjectsByUID([]port.ProjectRef{
		{UID: "project-1", ParentUID: "unreachable-1"},
	})
	// Unreachable rather than absent: only a shortfall a later pass could
	// resolve is worth withholding a row over.
	projects.SetRefErrorFor("unreachable-1", errors.New("no responders available"))
	projector := NewProjector(formations, items, projects, publisher)

	formation, err := formations.Create(ctx, &model.Formation{ProjectUID: "project-1"})
	if err != nil {
		t.Fatalf("seeding formation = %v", err)
	}
	if _, err := items.InsertMany(ctx, []*model.Item{
		{FormationUID: formation.UID, ItemKey: "a", Title: "Item A", Assignee: "jdoe"},
	}); err != nil {
		t.Fatalf("seeding items = %v", err)
	}

	if _, err := projector.Refresh(ctx, refOf(t, projects, "project-1"), Republish); err == nil {
		t.Fatal("Refresh() = nil error, want one")
	}

	if got := publisher.Count(); got != 0 {
		t.Errorf("published %d checklist documents, want 0", got)
	}
	if got := publisher.ItemCount(); got != 1 {
		t.Errorf("published %d item documents, want 1 — the assignee's list must not wait for the sweep", got)
	}
}

// The same holds on the scheduled path, where the row publishes anyway.
func TestNoReaderStillEmitsTheDirectParent(t *testing.T) {
	projector := NewProjector(nil, nil, nil, nil)

	chain, outcome := projector.ancestorChain(context.Background(),
		port.ProjectRef{UID: "project-1", ParentUID: "foundation-1"})

	if outcome != chainFinal {
		t.Error("with no reader wired there is nothing to retry, so the walk is final")
	}
	if len(chain) != 2 || chain[1] != "foundation-1" {
		t.Errorf("chain = %v, want the known direct parent kept — dropping it would scope worse "+
			"than before ancestry existed", chain)
	}
}

// Parentage is read fresh on every pass and nothing is memoized across passes,
// which is what makes a reparenting upstream arrive by the ordinary sweep
// rather than needing an invalidation step. A memo that survived here would
// mask exactly that.
func TestASecondPassSeesAReparenting(t *testing.T) {
	ctx := context.Background()
	projects := tree("project-1", "foundation-old", "root-1")
	projector := NewProjector(nil, nil, projects, nil)

	first, _ := projector.ancestorChain(ctx, refOf(t, projects, "project-1"))
	if len(first) < 2 || first[1] != "foundation-old" {
		t.Fatalf("chain = %v, want it under foundation-old to begin with", first)
	}

	// Reparent upstream, exactly as the project service would report it on the
	// next sweep.
	projects.SetProjectsByUID([]port.ProjectRef{
		{UID: "project-1", ParentUID: "foundation-new"},
		{UID: "foundation-new", ParentUID: "root-1"},
		{UID: "root-1"},
	})

	second, outcome := projector.ancestorChain(ctx, refOf(t, projects, "project-1"))
	if outcome != chainComplete {
		t.Fatalf("outcome is not chainComplete after reparenting; chain = %v", second)
	}
	if len(second) < 2 || second[1] != "foundation-new" {
		t.Errorf("chain = %v, want it under foundation-new — a stale memo would still say foundation-old", second)
	}
	for _, uid := range second {
		if uid == "foundation-old" {
			t.Errorf("chain = %v still carries the old foundation", second)
		}
	}
}

// One projector serves both the sweep and the write-triggered refresh, so the
// counter and anything else it holds are touched concurrently. Meaningful only
// under -race, which this repository's commit gate runs.
func TestConcurrentResolutionIsRaceFree(t *testing.T) {
	ctx := context.Background()
	projects := tree("project-1", "foundation-1", "root-1")
	projector := NewProjector(nil, nil, projects, nil)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			projector.ancestorChain(ctx, port.ProjectRef{UID: "project-1", ParentUID: "foundation-1"})
			projector.PartialChains()
		}()
	}
	wg.Wait()
}

// A projector with no reader is the deployment shape a nil publisher also
// serves. It must not panic, and a project with no parent is still a complete
// chain of one.
func TestNoReaderStillProducesTheProjectsOwnChain(t *testing.T) {
	projector := NewProjector(nil, nil, nil, nil)

	chain, outcome := projector.ancestorChain(context.Background(), port.ProjectRef{UID: "project-1"})

	if outcome != chainComplete {
		t.Error("outcome is not chainComplete for a parentless project")
	}
	if len(chain) != 1 || chain[0] != "project-1" {
		t.Errorf("chain = %v, want [project-1]", chain)
	}
}

// chainIs reports whether a resolved chain matches, order included.
func chainIs(chain []string, want ...string) bool {
	if len(chain) != len(want) {
		return false
	}
	for i := range want {
		if chain[i] != want[i] {
			return false
		}
	}
	return true
}

// A project event must not narrow a chain that is already published.
//
// The listener reaches the publish through the same reconcile the sweep uses,
// so the posture cannot be read off the caller. Deciding it by path put every
// project.updated event on the publish-what-resolved side: an ancestor that
// briefly stops answering during an ordinary edit would replace a complete
// chain with a prefix, dropping the row out of every foundation queue above the
// break until the next sweep — the exact under-report this feature exists to
// remove, reintroduced by the fix for it.
//
// The posture comes from whether the checklist was created in this pass
// instead, which is the only case with no earlier document to lose.
func TestAProjectEventDoesNotNarrowAnAlreadyPublishedChain(t *testing.T) {
	ctx := context.Background()
	projects := tree("child-1", "foundation-1", "root-1")
	r, _, publisher := newReconcilerWithIndex(t, projects)
	child := port.ProjectRef{
		UID: "child-1", Slug: "child-1",
		SubStage: model.StageFormationEngaged, ParentUID: "foundation-1",
	}

	// The event that creates the checklist. Nothing is published for it yet, so
	// the whole chain resolves and goes out.
	if report := r.ReconcileProject(ctx, child, TriggerListener); report.Created != 1 {
		t.Fatalf("arranging the checklist: created = %d, want 1", report.Created)
	}
	first := publisher.Latest(child.UID)
	if first == nil {
		t.Fatal("the creating event published no row")
	}
	if !chainIs(first.AncestorUIDs, "child-1", "foundation-1", "root-1") {
		t.Fatalf("published chain = %v, want the full chain", first.AncestorUIDs)
	}
	publishesBefore := publisher.Count()

	// The ancestors stop answering, which is all a transient project-service
	// failure looks like from here.
	projects.SetRefError(errors.New("project service unreachable"))

	report := r.ReconcileProject(ctx, child, TriggerListener)

	if got := publisher.Count() - publishesBefore; got != 0 {
		t.Errorf("published %d rows over a complete chain, want 0", got)
	}
	if latest := publisher.Latest(child.UID); !chainIs(latest.AncestorUIDs, "child-1", "foundation-1", "root-1") {
		t.Errorf("chain in the index = %v, want it left as published", latest.AncestorUIDs)
	}
	if report.ProjectionFailed != 1 {
		t.Errorf("ProjectionFailed = %d, want 1 — a withheld row is a shortfall to find, not a silent skip",
			report.ProjectionFailed)
	}
}

// The creating pass is the one publish with nothing to lose.
//
// Its counterpart above withholds; this one must not, or a new project whose
// ancestor is unreachable gets no queue row at all until the next sweep —
// absent from every foundation rather than present under fewer.
func TestTheCreatingPassPublishesWhateverResolved(t *testing.T) {
	ctx := context.Background()
	projects := tree("child-1", "foundation-1", "root-1")
	projects.SetRefError(errors.New("project service unreachable"))
	r, _, publisher := newReconcilerWithIndex(t, projects)
	child := port.ProjectRef{
		UID: "child-1", Slug: "child-1",
		SubStage: model.StageFormationEngaged, ParentUID: "foundation-1",
	}

	report := r.ReconcileProject(ctx, child, TriggerListener)

	if report.Created != 1 {
		t.Fatalf("created = %d, want 1", report.Created)
	}
	published := publisher.Latest(child.UID)
	if published == nil {
		t.Fatal("no row published for a checklist created with an unreachable ancestor")
	}
	if !chainIs(published.AncestorUIDs, "child-1", "foundation-1") {
		t.Errorf("chain = %v, want the resolved prefix [child-1 foundation-1]", published.AncestorUIDs)
	}
}

// A chain that cannot get any longer must not hold the row back.
//
// Withholding is a wait for a better answer, and the structural bounds do not
// have one: the depth cap and a cycle are properties of the data, so every
// later walk returns the identical prefix. Withholding for them would not
// protect the row's scope, it would stop the document updating at all — counts,
// stage, name and dates included — for as long as the shape persisted, with no
// retry, sweep or repair command able to clear it.
func TestAChainStoppedByTheDepthCapStillPublishesOnRepublish(t *testing.T) {
	ctx := context.Background()
	deeper := make([]string, 0, maxAncestorDepth+3)
	for i := range maxAncestorDepth + 3 {
		deeper = append(deeper, fmt.Sprintf("p-%d", i))
	}
	projects := tree(deeper...)
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	publisher := mock.NewIndexerPublisher()
	projector := NewProjector(formations, items, projects, publisher)

	if _, err := formations.Create(ctx, &model.Formation{ProjectUID: deeper[0]}); err != nil {
		t.Fatalf("seeding formation = %v", err)
	}

	published, err := projector.Refresh(ctx, refOf(t, projects, deeper[0]), Republish)

	if err != nil {
		t.Fatalf("Refresh() = %v, want no error — the chain is as long as it will ever be", err)
	}
	if !published {
		t.Error("published = false, want true — withholding here freezes the row for good")
	}
	if got := publisher.Count(); got != 1 {
		t.Errorf("published %d checklist documents, want 1", got)
	}
	if got := projector.PartialChains(); got != 1 {
		t.Errorf("PartialChains() = %d, want 1 — a capped chain is still a shortfall to report", got)
	}
}

// A deleted ancestor must not freeze the row either.
//
// This is the same hazard the depth cap has, arriving by a different route: the
// project service answers, and keeps answering, that the ancestor is gone. A
// child left pointing at it would be withheld on every republish for as long as
// the reference stood, so counts and stage would stop updating with no retry,
// sweep or repair able to clear it. Only reparenting would, and nothing would
// say that was needed.
func TestADeletedAncestorStillPublishesOnRepublish(t *testing.T) {
	ctx := context.Background()
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	publisher := mock.NewIndexerPublisher()
	projects := mock.NewProjectReader()
	projects.SetProjectsByUID([]port.ProjectRef{
		{UID: "project-1", ParentUID: "deleted-1"},
		// deleted-1 is unseeded, so the reader answers not-found for it.
	})
	projector := NewProjector(formations, items, projects, publisher)

	if _, err := formations.Create(ctx, &model.Formation{ProjectUID: "project-1"}); err != nil {
		t.Fatalf("seeding formation = %v", err)
	}

	published, err := projector.Refresh(ctx, refOf(t, projects, "project-1"), Republish)

	if err != nil {
		t.Fatalf("Refresh() = %v, want no error — the answer will not improve on a retry", err)
	}
	if !published {
		t.Error("published = false, want true — withholding here freezes the row for good")
	}
	doc := publisher.Latest("project-1")
	if doc == nil {
		t.Fatal("no document published")
	}
	if !chainIs(doc.AncestorUIDs, "project-1", "deleted-1") {
		t.Errorf("ancestor_uids = %v, want the known prefix kept", doc.AncestorUIDs)
	}
}
