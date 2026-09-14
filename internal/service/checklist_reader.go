// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"strconv"

	"github.com/google/uuid"
	goa "goa.design/goa/v3/pkg"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// defaultActivityPageLimit mirrors the postgres activity repository's own
// default, applied here too so a caller that sends limit=0 (Goa's zero value
// when the query param is omitted) gets a page rather than an empty result.
const defaultActivityPageLimit = 20

// The activity route's two not-found messages. Both are 404 with the same
// body shape, so the message is the only thing that tells a caller which of
// the two it got — which makes these part of the wire contract rather than
// log text, and is why they are constants asserted by tests rather than
// literals written at each return.
//
// Adding a discriminator field to NotFoundError instead would have changed
// the 404 body for the formation case, which the unchanged consumer must not
// see; and Goa needs an attribute tagged Meta("struct:error:name") to tell
// two custom errors on one method apart, which that type does not carry.
const (
	formationNotFoundMessage = "no formation exists for this project"
	itemNotFoundMessage      = "no such item in this formation"
)

// GetFormation assembles the whole checklist in one response: sections,
// items, progress and readiness. Items are carried with only functional
// fields — every label, icon and composed detail line belongs to the
// browser, keyed on item_key.
func (s *Service) GetFormation(ctx context.Context, p *svc.GetFormationPayload) (*svc.FormationChecklist, error) {
	formation, err := s.formations.GetByProject(ctx, p.ProjectUID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, &svc.NotFoundError{Code: "404", Message: "no formation exists for this project"}
		}
		return nil, err
	}

	items, err := s.items.ListByFormation(ctx, formation.UID)
	if err != nil {
		return nil, err
	}

	// Progress and gate readiness come from the items just loaded, not from
	// two more aggregate queries: every input is already in the slice, and
	// deriving them here keeps the tally consistent with the items shipped
	// beside it in the same response.
	counts := countsFromItems(items)
	gateTotal, gateOutstanding := gateSummaryFromItems(items)

	var announcementDate *string
	if s.projects != nil {
		settings, err := s.projects.GetSettings(ctx, p.ProjectUID)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
		if settings != nil {
			announcementDate = settings.AnnouncementDate
		}
	}

	return &svc.FormationChecklist{
		ProjectUID:      formation.ProjectUID,
		TemplateUID:     formation.TemplateUID.String(),
		TemplateVersion: formation.TemplateVersion,
		Lifecycle:       string(formation.Lifecycle),
		Sections:        sectionsFromFormation(formation),
		Items:           itemsToWire(items),
		Progress:        progressFromCounts(counts),
		IsActivating:    isActivating(gateTotal, gateOutstanding, announcementDate),
	}, nil
}

// GetFormationActivity returns the checklist's activity feed, newest first.
// The feed covers checklist changes only: status changes, assignment,
// notes, links, skip reasons and template work. Permission changes never
// appear here — nothing keeps a history of them, since each save overwrites
// the previous state — and that absence is conveyed by the method's own doc
// comment in the design rather than left for the browser to guess at.
//
// An item_uid narrows the feed to one item's own history. Absent, this is
// byte-identical to what it has always returned: the parameter is optional
// and the consumer that pages the whole feed keeps working untouched.
func (s *Service) GetFormationActivity(ctx context.Context, p *svc.GetFormationActivityPayload) (*svc.FormationActivityPage, error) {
	formation, err := s.formations.GetByProject(ctx, p.ProjectUID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, &svc.NotFoundError{Code: "404", Message: formationNotFoundMessage}
		}
		return nil, err
	}

	itemUID, err := s.resolveActivityItemFilter(ctx, formation.UID, p.ItemUID)
	if err != nil {
		return nil, err
	}

	cursor := ""
	if p.Cursor != nil {
		cursor = *p.Cursor
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultActivityPageLimit
	}

	entries, nextCursor, err := s.activity.List(ctx, formation.UID, itemUID, cursor, limit)
	if err != nil {
		return nil, err
	}

	return &svc.FormationActivityPage{
		Entries:    activityToWire(entries),
		NextCursor: &nextCursor,
	}, nil
}

// resolveActivityItemFilter turns the optional item reference on the payload
// into the filter the repository takes, having first established that the item
// is one this formation actually has.
//
// The existence check is what keeps an empty page meaning exactly "no
// history". Without it an unknown identifier reads as an empty feed, which
// reintroduces the ambiguity the filter exists to remove — moved from
// truncation to identity.
//
// One branch reports both "no such item anywhere" and "an item, but another
// formation's". Deliberately one, and deliberately the same error: two
// distinguishable responses would make this route an existence oracle for
// items in projects the caller cannot see. The caller learns only that the
// item is not in the project it named, which it was already authorized to
// know.
func (s *Service) resolveActivityItemFilter(
	ctx context.Context, formationUID uuid.UUID, raw *string,
) (*uuid.UUID, error) {
	if raw == nil {
		return nil, nil
	}

	itemUID, err := uuid.Parse(*raw)
	if err != nil {
		// Unreachable over HTTP: the design declares item_uid as a UUID, so a
		// malformed value is refused at decode and never arrives here. Kept
		// because the two cases it must never become are a 500 and a silent
		// fall back to the unfiltered feed, and a direct caller of this method
		// would otherwise reach the second. goa.InvalidFormatError encodes as
		// 400, the same status decode gives.
		return nil, goa.InvalidFormatError("item_uid", *raw, goa.FormatUUID, err)
	}

	item, err := s.items.Get(ctx, itemUID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, &svc.NotFoundError{Code: "404", Message: itemNotFoundMessage}
		}
		return nil, err
	}
	if item.FormationUID != formationUID {
		return nil, &svc.NotFoundError{Code: "404", Message: itemNotFoundMessage}
	}

	return &itemUID, nil
}

// sectionsFromFormation reads section key, title and position from the
// checklist's own snapshot rather than from the pinned template: an item
// carries only its section_key, and the template can gain a section an
// upgrade has already added items for — this snapshot is what the upgrade job
// keeps in step with those items, so every section_key an item carries is
// guaranteed to appear here.
func sectionsFromFormation(f *model.Formation) []*svc.FormationSection {
	out := make([]*svc.FormationSection, 0, len(f.Sections))
	for i, sec := range f.Sections {
		out = append(out, &svc.FormationSection{
			Key:      sec.Key,
			Title:    sec.Title,
			Position: i,
		})
	}
	return out
}

func itemsToWire(items []*model.Item) []*svc.FormationItem {
	out := make([]*svc.FormationItem, 0, len(items))
	for _, it := range items {
		out = append(out, itemToWire(it))
	}
	return out
}

// itemETag renders an item's version for the ETag response header, so a
// caller holding a mutation's result already holds the If-Match for its next
// write. Bare digits: if_match is an Int64, so a quoted entity-tag echoed
// straight back would be refused at decode time.
func itemETag(item *svc.FormationItem) *string {
	etag := strconv.FormatInt(item.Version, 10)
	return &etag
}

func itemToWire(it *model.Item) *svc.FormationItem {
	wire := &svc.FormationItem{
		UID:            it.UID.String(),
		ItemKey:        it.ItemKey,
		SectionKey:     it.SectionKey,
		Position:       it.Position,
		Title:          it.Title,
		Gate:           it.Gate,
		RequiresWriter: it.RequiresWriter,
		StatusSource:   string(it.StatusSource),
		IsRequired:     it.IsRequired,
		ChecklistType:  string(it.ChecklistType),
		Status:         string(it.Status),
		Version:        it.Revision,
	}
	if it.OwnerTeam != "" {
		wire.OwnerTeam = &it.OwnerTeam
	}
	if it.ActionLink != "" {
		wire.ActionLink = &it.ActionLink
	}
	if it.EvidenceLink != "" {
		wire.EvidenceLink = &it.EvidenceLink
	}
	if it.Assignee != "" {
		wire.Assignee = &it.Assignee
	}
	if it.Note != "" {
		wire.Note = &it.Note
	}
	if it.SkipReason != "" {
		wire.SkipReason = &it.SkipReason
	}
	if it.DueDate != nil {
		due := it.DueDate.Format("2006-01-02")
		wire.DueDate = &due
	}
	if it.PlatformCheck != nil {
		wire.PlatformCheck = &svc.FormationPlatformCheck{
			ResourceType: &it.PlatformCheck.ResourceType,
			MinCount:     &it.PlatformCheck.MinCount,
		}
	}
	if it.ResolvedRef != nil {
		wire.ResolvedRef = &svc.FormationResolvedRef{
			Type: &it.ResolvedRef.Type,
			UID:  &it.ResolvedRef.UID,
		}
	}
	if len(it.SubItems) > 0 {
		wire.SubItems = make([]*svc.FormationSubItem, 0, len(it.SubItems))
		for _, si := range it.SubItems {
			wire.SubItems = append(wire.SubItems, &svc.FormationSubItem{
				Key:    si.Key,
				Title:  si.Title,
				Status: string(si.Status),
			})
		}
	}
	return wire
}

func activityToWire(entries []*model.ActivityEntry) []*svc.FormationActivityEntry {
	out := make([]*svc.FormationActivityEntry, 0, len(entries))
	for _, e := range entries {
		wire := &svc.FormationActivityEntry{
			Ulid:   e.ULID,
			Actor:  e.Actor,
			SetBy:  string(e.SetBy),
			Action: e.Action,
			Before: e.Before,
			After:  e.After,
			At:     e.At.Format("2006-01-02T15:04:05Z07:00"),
		}
		if e.ItemUID != nil {
			uid := e.ItemUID.String()
			wire.ItemUID = &uid
		}
		out = append(out, wire)
	}
	return out
}
