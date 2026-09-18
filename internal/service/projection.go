// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// Projector publishes the queue's view of a checklist.
//
// It is driven by the reconcile rather than by each mutation, and that is the
// design rather than a simplification. A projection published only on change has
// to be repaired when a publish is lost, which means either a tool nobody runs
// or a backfill nobody remembers; republishing every checklist on every sweep
// makes the repair path and the ordinary path the same code. The cost is one
// checklist write plus one item write per item per tick for checklists that did
// not change — not the single document per project this cost the sweep before
// item documents existed.
type Projector struct {
	formations port.FormationRepository
	items      port.ItemRepository
	projects   port.ProjectReader
	publisher  port.IndexerPublisher

	// partialChains counts rows whose parentage resolved only partly, on
	// either publish path. Carried here rather than left to the index because
	// a row scoped to less than its true parentage is invisible in the result
	// — it simply does not appear under a foundation it belongs to, which
	// looks exactly like it not existing. This is the only signal that says
	// otherwise.
	partialChains atomic.Int64
}

// PublishPosture says whether a document for this row can already exist, which
// decides what happens when the project's parentage cannot be fully resolved.
//
// The question is whether this publish has anything to lose, not which path is
// asking. A row whose checklist was created in this pass has no earlier
// document to fall back on, so it publishes what it resolved — present under
// fewer foundations beats absent from all of them. Every other publish is
// replacing a document that may already carry the full chain, and a shorter
// chain over it would drop the row out of its foundation's queue until the next
// sweep: the same silent under-report this whole change exists to remove,
// reintroduced as a side effect of the fix.
//
// Deliberately not inferred from the caller. The sweep and the project event
// listener reach the publish through the same reconcile, so a listener handling
// project.updated for a row that has existed for months arrives at the same
// line as a sweep that has just created one.
type PublishPosture int

const (
	// FirstPublish is a checklist created in this pass, so no document for it
	// can exist yet. Publishes whatever chain resolved.
	FirstPublish PublishPosture = iota

	// Republish is every other publish. Skips rather than narrowing the scope
	// of a document already in the index.
	Republish
)

// maxAncestorDepth bounds the upward walk.
//
// Headroom over a measurement, not a guess: the deepest real chain in
// production is five hops, over a project tree that is five levels deep in
// total. Ten leaves room for the hierarchy to roughly double before anything
// here needs revisiting, while still bounding the cost of a chain that data
// corruption has made pathological.
//
// Reaching it is treated as partial resolution, not as an error — see
// ancestorChain.
const maxAncestorDepth = 10

// NewProjector wires a projector. A nil publisher disables projection, which is
// how a deployment with no NATS still serves its read and write routes.
func NewProjector(
	formations port.FormationRepository,
	items port.ItemRepository,
	projects port.ProjectReader,
	publisher port.IndexerPublisher,
) *Projector {
	return &Projector{formations: formations, items: items, projects: projects, publisher: publisher}
}

// Refresh republishes one project's queue row, reporting whether it published.
//
// The bool is not decoration. There is nothing to publish when no publisher is
// wired or the project has no checklist yet, and neither is a failure — a
// project at a formation stage whose checklist has not been created is a row the
// queue is not supposed to have, since the queue searches checklists rather than
// projects. But a caller counting successes cannot tell that from a publish, and
// a sweep of three projects holding two checklists reported three rows
// republished, which is a number nobody can reconcile against the index.
//
// The project facts are read fresh on every call and never stored. That is why
// this is safe to run on a ticker: there is no cached copy to invalidate, so the
// only staleness is the age of the last sweep.
func (p *Projector) Refresh(ctx context.Context, project port.ProjectRef, posture PublishPosture) (bool, error) {
	if p.publisher == nil {
		return false, nil
	}

	formation, err := p.formations.GetByProject(ctx, project.UID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		return false, err
	}

	items, err := p.items.ListByFormation(ctx, formation.UID)
	if err != nil {
		return false, err
	}

	name, announcementDate := p.projectFacts(ctx, project)

	// withheld means the checklist row is deliberately left as it was
	// published, because this pass could only build a shorter chain than the
	// document already in the index carries.
	//
	// Only a retryable shortfall is worth withholding over. A chain stopped by
	// the depth cap or a cycle is as long as it will ever be, so holding the
	// row back would not be waiting for a better answer — it would freeze every
	// other field on the document, counts and stage included, for good.
	ancestors, outcome := p.ancestorChain(ctx, project)
	withheld := outcome == chainRetryable && posture == Republish
	if outcome != chainComplete {
		p.partialChains.Add(1)
		if withheld {
			slog.WarnContext(ctx, "leaving the queue row as published: parentage resolved to less than the document already in the index may carry",
				"project_uid", project.UID, "resolved_depth", len(ancestors))
		} else {
			slog.WarnContext(ctx, "publishing a queue row scoped to less than its full parentage",
				"project_uid", project.UID, "resolved_depth", len(ancestors))
		}
	}

	if !withheld {
		if err := p.publisher.PublishFormation(ctx, buildProjection(
			formation, items, project, name, announcementDate, ancestors, time.Now(),
		)); err != nil {
			return false, err
		}
	}

	// One item document per item, alongside the checklist document above, in
	// a single batch so a checklist of N items costs one flush rather than N.
	// Best-effort like the checklist publish: a failed item is repaired by
	// the next sweep, and must not fail the checklist publish that already
	// succeeded or the projects after this one in the sweep.
	//
	// A total failure is returned as an error rather than as a false, even
	// though the checklist document itself landed. The caller reads an error
	// as a failed projection and a false as a project holding no checklist
	// to publish, so a false here would leave a stale index counted as
	// neither. Returning it stays non-fatal to the sweep: the caller logs,
	// counts, and moves on to the next project.
	//
	// project and name are already resolved above for the checklist
	// document; reusing them here costs nothing further, no second project
	// lookup.
	//
	// Published even when the checklist row above was withheld. Item documents
	// carry no ancestry at all, so nothing about them is uncertain when
	// parentage fails to resolve — and withholding them would stall the
	// assignee's list until the next sweep, which is the one thing the
	// write-triggered path exists to prevent.
	itemDocs := buildItemProjections(formation, items, project, name)
	if len(itemDocs) > 0 {
		if err := p.publisher.PublishItems(ctx, itemDocs); err != nil {
			return false, fmt.Errorf("publishing this checklist's item rows: %w", err)
		}
	}

	// Reported as an error rather than as a bare false, and the distinction is
	// not cosmetic: a false means the project holds no checklist, which the
	// caller counts as "nothing to do" and logs at debug. A row withheld
	// because its parentage would have regressed is a shortfall somebody has
	// to be able to find, and it belongs in the same count as any other
	// projection that did not land. The item rows above still published, which
	// matches how a failed item batch is reported after the checklist row
	// succeeded.
	if withheld {
		return false, fmt.Errorf("parentage for project %s resolved to %d of its chain; "+
			"leaving the published row in place rather than narrowing its scope",
			project.UID, len(ancestors))
	}
	return true, nil
}

// projectFacts resolves the two facts the list reply does not carry: the display
// name and the announcement date.
//
// Neither failure stops the row being published, and both degrade to something
// usable rather than to nothing. A row absent from the queue because its name
// could not be read is indistinguishable from a project the viewer may not see,
// which is the one failure mode this screen must not have — so the name falls
// back to the slug, which the list reply always carries.
func (p *Projector) projectFacts(ctx context.Context, project port.ProjectRef) (name, announcementDate string) {
	name = project.Slug
	if p.projects == nil {
		return name, ""
	}

	// One request per project per sweep. Acceptable here in a way it would not
	// be on a request path: this runs on a ticker in the background, and the
	// queue itself never reaches this service — it searches the index. So the
	// cost is sweep duration, not page latency. It still wants removing, by
	// carrying the name on the list reply upstream.
	if resolved, err := p.projects.Name(ctx, project.UID); err == nil && resolved != "" {
		name = resolved
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		slog.WarnContext(ctx, "could not resolve the project name for its queue row; using the slug",
			"project_uid", project.UID, "error", err)
	}

	settings, err := p.projects.GetSettings(ctx, project.UID)
	switch {
	case err != nil && !errors.Is(err, domain.ErrNotFound):
		slog.WarnContext(ctx, "could not read the announcement date for its queue row; "+
			"the row is published without one and sorts last",
			"project_uid", project.UID, "error", err)
	case settings != nil && settings.AnnouncementDate != nil:
		announcementDate = *settings.AnnouncementDate
	}

	return name, announcementDate
}

// PartialChains reports how many rows have been published scoped to less than
// their full parentage, or withheld for the same reason.
func (p *Projector) PartialChains() int64 {
	return p.partialChains.Load()
}

// chainOutcome says how far the upward walk got and, where it stopped short,
// whether asking again could get further.
//
// The distinction decides whether a short chain is worth withholding a publish
// over. Withholding is only ever a wait for a better answer, so it needs one to
// be possible.
type chainOutcome int

const (
	// chainComplete reached the platform root.
	chainComplete chainOutcome = iota

	// chainFinal stopped at a structural bound — the depth cap or a cycle.
	// Both are properties of the data rather than of this attempt, so every
	// later walk returns the same prefix and there is nothing to wait for.
	chainFinal

	// chainRetryable stopped because the project service could not answer for
	// an ancestor. The next pass may well get further. An ancestor it answered
	// about by saying there is no such project is chainFinal instead — that
	// answer does not change on a retry.
	chainRetryable
)

// ancestorChain walks from the project up to the platform root, returning the
// chain nearest-first and how the walk ended.
//
// The project's own UID comes first. That is what makes scoping to a foundation
// return the foundation's own row as well as everything beneath it, which is
// the behaviour the queue's nested rendering and its Foundation type value both
// assume.
//
// One lookup per generation, which cannot be batched: a grandparent's identity
// is not known until its child has been read. Chains are three to five hops in
// production, so this is a handful of requests per row on a daily sweep, and
// never on a path anybody waits on — the queue is one search against the index
// and does not reach this service at all.
//
// Nothing is memoized. A memo would have to outlive the refresh that populated
// it to save anything, since a single chain never visits the same project
// twice, and parentage that outlives its refresh is the stale-scoping failure
// this design rules out.
//
// Both bounds — a project seen twice, and the depth cap — return what has been
// resolved so far rather than an error, and both are chainFinal: bad data, not
// a bad moment. So is a deleted ancestor. Only an ancestor the project service
// could not answer for is worth waiting on.
func (p *Projector) ancestorChain(ctx context.Context, project port.ProjectRef) (chain []string, outcome chainOutcome) {
	chain = []string{project.UID}
	if p.projects == nil {
		// No reader wired, which is the same deployment shape a nil publisher
		// serves. The direct parent is still known from the ref in hand and is
		// still emitted — dropping it would make this shape scope worse than it
		// did before ancestry existed, which no degradation is allowed to do.
		if project.ParentUID != "" {
			return append(chain, project.ParentUID), chainFinal
		}
		return chain, chainComplete
	}

	visited := map[string]bool{project.UID: true}
	next := project.ParentUID

	for depth := 0; next != ""; depth++ {
		if depth >= maxAncestorDepth {
			slog.WarnContext(ctx, "stopped resolving parentage at the depth cap",
				"project_uid", project.UID, "cap", maxAncestorDepth)
			return chain, chainFinal
		}
		if visited[next] {
			slog.WarnContext(ctx, "stopped resolving parentage at a cycle",
				"project_uid", project.UID, "repeated_uid", next)
			return chain, chainFinal
		}
		visited[next] = true

		// Appended before the lookup, and that is deliberate: this UID came
		// off a project already read, so the row genuinely sits beneath it
		// whether or not the project behind it can be read. Dropping it on a
		// failed lookup would discard a known-good ancestor along with the
		// unknown ones above it.
		chain = append(chain, next)

		ref, err := p.projects.GetRef(ctx, next)
		if err != nil {
			// A not-found is a successful read of a project that is not there,
			// which ProjectClient.GetRef keeps deliberately distinct from an
			// upstream that could not answer. A deleted ancestor reads the same
			// way on every later pass, so waiting for it holds the row back for
			// good.
			if errors.Is(err, domain.ErrNotFound) {
				slog.WarnContext(ctx, "an ancestor no longer exists; scoping the row to what resolved",
					"project_uid", project.UID, "ancestor_uid", next)
				return chain, chainFinal
			}
			slog.WarnContext(ctx, "could not read an ancestor while resolving parentage; scoping the row to what resolved",
				"project_uid", project.UID, "ancestor_uid", next, "error", err)
			return chain, chainRetryable
		}
		next = ref.ParentUID
	}

	// An empty parent is how the platform root answers, so the walk stops
	// there on its own. The bounds above are guards against bad data, not the
	// ordinary way out.
	return chain, chainComplete
}

// formationAccessRelation is the relation a caller must hold on the project to
// read a formation row: auditor, and never viewer.
//
// The platform's FGA model defines project#viewer with a `user:*` grant — every
// authenticated user, deliberately, because a public project's existence is
// public. A formation checklist is not: it carries who is assigned what, which
// items are blocked, and how close the project is to being announced, for
// projects that may be confidential. Declaring against viewer would publish all
// of that to anyone holding an account.
//
// This was checked against the live dev index rather than assumed, and the index
// disagrees with it: project documents stamp viewer. That is why this is a named
// constant carrying its reasoning rather than a string at the call site — the
// value here is the fail-closed one, and it is meant to survive somebody noticing
// the inconsistency and "fixing" it in the wrong direction.
const formationAccessRelation = "auditor"

// buildProjection turns a checklist and its items into the one queue row that
// represents them.
//
// The queue is answerable by a single access-filtered search with no per-row
// request, which means every number the screen shows has to be a field on this
// document. The counts are therefore published rather than computed by the
// browser over items — the items are not in the document at all, and publishing
// them would put every note and assignee into a search index.
//
// project carries the facts this service does not own. A zero value is not an
// error: the sweep publishes what it has rather than skipping the row, because a
// row missing from the queue is indistinguishable from a project nobody may see.
func buildProjection(
	formation *model.Formation,
	items []*model.Item,
	project port.ProjectRef,
	projectName string,
	announcementDate string,
	ancestorUIDs []string,
	now time.Time,
) *port.FormationProjection {
	counts := countsFromItems(items)
	gateTotal, gateOutstanding := gateSummaryFromItems(items)

	// Both halves of readiness are published, under names that say which is
	// which. The queue needs the gates-only half on its own: a project whose
	// gates are cleared but which has no announcement date yet is exactly the
	// row staff are looking for, and a single combined flag cannot express it.
	//
	// The names matter more than they look. This service already serves
	// is_activating on the checklist page meaning gates *and* date, so
	// publishing a field of that name meaning only half of it would have the
	// same word mean two things across two screens. gates_cleared is the new
	// name; is_activating keeps the meaning it already has.
	gatesCleared := gateTotal > 0 && gateOutstanding == 0

	var datePtr *string
	if announcementDate != "" {
		datePtr = &announcementDate
	}

	doc := &port.FormationProjection{
		FormationUID:      formation.UID.String(),
		ProjectUID:        formation.ProjectUID,
		ProjectName:       projectName,
		AnnouncementDate:  announcementDate,
		Lifecycle:         string(formation.Lifecycle),
		GatesCleared:      gatesCleared,
		IsActivating:      isActivating(gateTotal, gateOutstanding, datePtr),
		NotStarted:        counts[model.StatusNotStarted],
		InProgress:        counts[model.StatusInProgress],
		Blocked:           counts[model.StatusBlocked],
		Done:              counts[model.StatusDone],
		Skipped:           counts[model.StatusSkipped],
		BlockedItemTitles: blockedItemTitles(items),
		StalledCount:      stalledCount(items, now),
		Assignees:         assigneesOf(items),
		AccessRelation:    formationAccessRelation,

		// No three-way Type field is stored or published. The browser derives
		// Foundation / Project / Child project from these two signals, and
		// indenting a row only when its parent is also in the results is a
		// decision that needs the whole result set — which the browser has and
		// this publisher, looking at one checklist, does not.
		ProjectSlug:  project.Slug,
		IsFoundation: project.IsFoundation,
		ParentUID:    project.ParentUID,
		SubStage:     project.SubStage,

		// The whole chain, alongside — not instead of — the direct parent
		// above. The Type column reads ParentUID and must keep seeing exactly
		// one project there; scoping reads the chain.
		AncestorUIDs: ancestorUIDs,
	}

	return doc
}

// stalledCount returns the number of assigned, non-terminal items that have
// gone quiet, following the same rollup pattern as blockedItemTitles.
//
// "Stalled" is defined here, in one place, and never recomputed elsewhere:
//   - The item is assigned (Assignee is non-empty).
//   - The item is not in a terminal state (not done, not skipped).
//   - The item is past its DueDate. Items with no DueDate are not counted as
//     stalled by this definition — a fallback for undeadlined assigned work
//     requires an assigned_at timestamp that does not yet exist on the model.
//
// Returns nil when no items are assigned at all. The caller must distinguish
// nil ("nothing to chase") from a pointer to zero ("assigned work, none stalled")
// — see FormationProjection.StalledCount.
func stalledCount(items []*model.Item, now time.Time) *int {
	hasAssigned := false
	count := 0
	for _, item := range items {
		if item.Assignee == "" {
			continue
		}
		hasAssigned = true
		if item.Status == model.StatusDone || item.Status == model.StatusSkipped {
			continue
		}
		// DueDate is stored as a date-only value (midnight UTC). Normalize
		// "today" to UTC before comparing so the result is the same
		// regardless of the process timezone: a non-UTC now.Location()
		// would build a different midnight than the stored value, marking
		// an item due today as stalled hours early (UTC-7 midnight is
		// 07:00Z, already after the stored 00:00Z of the due date).
		if item.DueDate != nil {
			nowUTC := now.UTC()
			startOfTodayUTC := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 0, 0, 0, 0, time.UTC)
			if startOfTodayUTC.After(*item.DueDate) {
				count++
			}
		}
	}
	if !hasAssigned {
		return nil
	}
	return &count
}

// blockedItemTitles lists the titles of blocked items, for the Blocking column.
//
// Titles only, never the assignee: this document is readable by everyone holding
// the project's audit relation, and who is stuck on what is a different
// disclosure from what is stuck. Sorted so that republishing an unchanged
// checklist produces an unchanged document — the items arrive ordered by section
// and position, but a status change reorders which of them are blocked, and a
// document that differs only in array order is a needless index write.
func blockedItemTitles(items []*model.Item) []string {
	titles := make([]string, 0, len(items))
	for _, item := range items {
		if item.Status == model.StatusBlocked {
			titles = append(titles, item.Title)
		}
	}
	sort.Strings(titles)
	return titles
}

// buildItemProjections turns a checklist's items into the per-item documents
// the Pending Actions query reads, one per item.
//
// project and projectName are the same two arguments buildProjection takes for
// the same reason: project.Slug is a list-reply fact, projectName is the one
// extra resolved fact the list reply does not carry (see projectFacts) — so
// this mirrors that signature rather than inventing its own shape for the
// same two facts.
//
// AccessRelation is set to formationAccessRelation for every item, explicitly
// and unconditionally — never viewer, regardless of the item's own status,
// gate, or any other content — matching the checklist projection's own
// relation exactly: an item is indexed the same way the checklist is.
func buildItemProjections(
	formation *model.Formation,
	items []*model.Item,
	project port.ProjectRef,
	projectName string,
) []*port.ItemProjection {
	out := make([]*port.ItemProjection, 0, len(items))
	for _, item := range items {
		var dueDate string
		if item.DueDate != nil {
			// Matches checklist_reader.go's own wire conversion, so the same
			// date reads identically on the checklist response and in this
			// index.
			dueDate = item.DueDate.Format(dueDateLayout)
		}

		subItems := make([]port.ItemProjectionSubItem, 0, len(item.SubItems))
		for _, s := range item.SubItems {
			subItems = append(subItems, port.ItemProjectionSubItem{
				Key:    s.Key,
				Title:  s.Title,
				Status: string(s.Status),
			})
		}

		out = append(out, &port.ItemProjection{
			ItemUID:        item.UID.String(),
			FormationUID:   formation.UID.String(),
			ProjectUID:     formation.ProjectUID,
			ProjectName:    projectName,
			ProjectSlug:    project.Slug,
			Lifecycle:      string(formation.Lifecycle),
			ItemKey:        item.ItemKey,
			Title:          item.Title,
			StatusSource:   string(item.StatusSource),
			Status:         string(item.Status),
			Gate:           item.Gate,
			RequiresWriter: item.RequiresWriter,
			DueDate:        dueDate,
			OwnerTeam:      item.OwnerTeam,
			ActionLink:     item.ActionLink,
			SubItems:       subItems,
			Assignee:       item.Assignee,
			AccessRelation: formationAccessRelation,
		})
	}
	return out
}

// assigneesOf collects the distinct assignees across the checklist, which is
// what the "Mine" filter matches on.
//
// Distinct because one person usually holds several items and the filter asks
// only whether they hold any. Sorted for the same reason the titles are.
func assigneesOf(items []*model.Item) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item.Assignee == "" || seen[item.Assignee] {
			continue
		}
		seen[item.Assignee] = true
		out = append(out, item.Assignee)
	}
	sort.Strings(out)
	return out
}
