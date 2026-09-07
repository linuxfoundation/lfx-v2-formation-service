// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// defaultActivityPageLimit mirrors the postgres activity repository's own
// default, applied here too so a caller that sends limit=0 (Goa's zero value
// when the query param is omitted) gets a page rather than an empty result.
const defaultActivityPageLimit = 20

// GetFormation assembles the whole checklist in one response (FR-031):
// sections, items, progress and readiness. Items are carried with only
// functional fields — every label, icon and composed detail line belongs to
// the browser, keyed on item_key (T041).
func (s *Service) GetFormation(ctx context.Context, p *svc.GetFormationPayload) (*svc.FormationChecklist, error) {
	formation, err := s.formations.GetByProject(ctx, p.ProjectUID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, &svc.NotFoundError{Code: "404", Message: "no formation exists for this project"}
		}
		return nil, err
	}

	template, err := s.templates.Get(ctx, formation.TemplateUID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	items, err := s.items.ListByFormation(ctx, formation.UID)
	if err != nil {
		return nil, err
	}

	counts, err := s.items.StatusCounts(ctx, formation.UID)
	if err != nil {
		return nil, err
	}

	gateTotal, gateOutstanding, err := s.items.GateSummary(ctx, formation.UID)
	if err != nil {
		return nil, err
	}

	var announcementDate *string
	if s.projects != nil {
		settings, err := s.projects.GetSettings(ctx, p.ProjectUID)
		if err != nil {
			return nil, err
		}
		announcementDate = settings.AnnouncementDate
	}

	return &svc.FormationChecklist{
		ProjectUID:      formation.ProjectUID,
		TemplateUID:     formation.TemplateUID.String(),
		TemplateVersion: formation.TemplateVersion,
		Lifecycle:       string(formation.Lifecycle),
		Sections:        sectionsFromTemplate(template),
		Items:           itemsToWire(items),
		Progress:        progressFromCounts(counts),
		IsActivating:    isActivating(gateTotal, gateOutstanding, announcementDate),
	}, nil
}

// GetFormationActivity returns the checklist's activity feed, newest first
// (FR-027). The feed covers checklist changes only: status changes,
// assignment, notes, links, skip reasons and template work. Permission
// changes never appear here — nothing keeps a history of them, since each
// save overwrites the previous state — and that absence is conveyed by the
// method's own doc comment in the design (T096) rather than left for the
// browser to guess at.
func (s *Service) GetFormationActivity(ctx context.Context, p *svc.GetFormationActivityPayload) (*svc.FormationActivityPage, error) {
	formation, err := s.formations.GetByProject(ctx, p.ProjectUID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, &svc.NotFoundError{Code: "404", Message: "no formation exists for this project"}
		}
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

	entries, nextCursor, err := s.activity.List(ctx, formation.UID, cursor, limit)
	if err != nil {
		return nil, err
	}

	return &svc.FormationActivityPage{
		Entries:    activityToWire(entries),
		NextCursor: &nextCursor,
	}, nil
}

// sectionsFromTemplate reads section key, title and position from the
// template rather than from the items, since an item carries only its
// section_key — the title and display order are template metadata. Returns
// an empty slice (never nil) when the template could not be loaded, so a
// checklist whose template was later archived still renders its items.
func sectionsFromTemplate(t *model.Template) []*svc.FormationSection {
	if t == nil {
		return []*svc.FormationSection{}
	}
	out := make([]*svc.FormationSection, 0, len(t.Sections))
	for i, sec := range t.Sections {
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
