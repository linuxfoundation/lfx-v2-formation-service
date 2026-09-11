// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"sort"

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
}

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
func (p *Projector) Refresh(ctx context.Context, project port.ProjectRef) (bool, error) {
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

	if err := p.publisher.PublishFormation(ctx, buildProjection(
		formation, items, project, name, announcementDate,
	)); err != nil {
		return false, err
	}

	// One item document per item, alongside the checklist document above, in
	// a single batch so a checklist of N items costs one flush rather than N.
	// Best-effort like the checklist publish: a failed item is repaired by
	// the next sweep, and must not fail the checklist publish that already
	// succeeded or the projects after this one in the sweep — unless every
	// item in this checklist failed, which the caller cannot tell from a
	// bare "true" without this reflecting it.
	itemDocs := buildItemProjections(formation, items)
	if len(itemDocs) > 0 {
		if err := p.publisher.PublishItems(ctx, itemDocs); err != nil {
			slog.WarnContext(ctx, "could not publish this checklist's item rows; the next sweep will retry",
				"formation_uid", formation.UID.String(), "error", err)
			return false, nil
		}
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
		FormationUID:       formation.UID.String(),
		ProjectUID:         formation.ProjectUID,
		ProjectName:        projectName,
		AnnouncementDate:   announcementDate,
		Lifecycle:          string(formation.Lifecycle),
		GatesCleared:       gatesCleared,
		IsActivating:       isActivating(gateTotal, gateOutstanding, datePtr),
		NotStarted:         counts[model.StatusNotStarted],
		InProgress:         counts[model.StatusInProgress],
		Blocked:            counts[model.StatusBlocked],
		AwaitingAcceptance: counts[model.StatusAwaitingAcceptance],
		Done:               counts[model.StatusDone],
		Skipped:            counts[model.StatusSkipped],
		BlockedItemTitles:  blockedItemTitles(items),
		Assignees:          assigneesOf(items),
		AccessRelation:     formationAccessRelation,

		// No three-way Type field is stored or published. The browser derives
		// Foundation / Project / Child project from these two signals, and
		// indenting a row only when its parent is also in the results is a
		// decision that needs the whole result set — which the browser has and
		// this publisher, looking at one checklist, does not.
		ProjectSlug:  project.Slug,
		IsFoundation: project.IsFoundation,
		ParentUID:    project.ParentUID,
		SubStage:     project.SubStage,
	}

	return doc
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
// AccessRelation is set to formationAccessRelation for every item, explicitly
// and unconditionally — never viewer, regardless of the item's own status,
// gate, or any other content — matching the checklist projection's own
// relation exactly: an item is indexed the same way the checklist is.
func buildItemProjections(
	formation *model.Formation,
	items []*model.Item,
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
			ItemKey:        item.ItemKey,
			Title:          item.Title,
			Status:         string(item.Status),
			Gate:           item.Gate,
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
