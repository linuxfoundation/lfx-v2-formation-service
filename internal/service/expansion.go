// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// The activity vocabulary for the two things that happen to a checklist without
// anyone asking. Named rather than inlined because both the expansion and the
// upgrade record them and the pair has to agree.
const (
	ActionTemplateExpanded = "template_expanded"
	ActionTemplateUpgraded = "template_upgraded"
)

// actorSystem is the actor on an entry no person caused. The activity feed
// requires an actor, and attributing an automatic expansion to whichever user
// happened to trigger the sweep would be a lie in the audit trail.
const actorSystem = "system"

// projectUIDPlaceholder is substituted into a template's action link once, at
// expansion, and the result is then fixed. Resolving it on every read would make
// a link change with a template edit, which is the opposite of the pinning rule
// the rest of expansion follows.
const projectUIDPlaceholder = "{{project.uid}}"

// dueRulePrefix is the only due rule shape: "announcement-30d" means thirty days
// before the project's announcement date. An enumerated stand-in rather than an
// expression, for the same reason match rules are.
const dueRulePrefix = "announcement-"

// Expander creates a project's checklist from the selected template.
type Expander struct {
	selector *TemplateSelector
	uow      port.UnitOfWork
	projects port.ProjectReader
}

// NewExpander wires an expander. projects may be nil: the announcement date is
// only used to compute due dates, so a missing reader leaves them unset rather
// than blocking creation of the checklist itself.
func NewExpander(selector *TemplateSelector, uow port.UnitOfWork, projects port.ProjectReader) *Expander {
	return &Expander{selector: selector, uow: uow, projects: projects}
}

// ExpandFor creates the checklist for a project if it does not have one.
//
// It reports whether it created anything, so the reconcile loop can log a sweep
// that changed nothing without that being indistinguishable from a sweep that
// created every checklist. An existing formation is success, not an error: the
// uniqueness constraint on project_uid is what makes the loop safe to run on
// every replica, so losing that race is the expected outcome rather than a
// failure to report.
func (e *Expander) ExpandFor(ctx context.Context, projectUID string) (bool, error) {
	tpl, err := e.SelectTemplate(ctx)
	if err != nil {
		return false, err
	}
	return e.ExpandWithTemplate(ctx, projectUID, tpl)
}

// SelectTemplate resolves the template a checklist would be created from.
//
// Separate from expansion so a caller creating many checklists resolves it once.
// The selector takes no project facts, so the answer is the same for every
// project in a sweep — resolving it per project meant one ListPublished, and one
// decode of the whole sections document, per project.
func (e *Expander) SelectTemplate(ctx context.Context) (*model.Template, error) {
	return e.selector.Select(ctx)
}

// ExpandWithTemplate creates the checklist from an already-resolved template.
func (e *Expander) ExpandWithTemplate(ctx context.Context, projectUID string, tpl *model.Template) (bool, error) {
	if projectUID == "" {
		return false, fmt.Errorf("expanding a checklist: %w", domain.ErrInvalidRequest)
	}
	if tpl == nil {
		return false, fmt.Errorf("expanding a checklist for %s with no template: %w", projectUID, domain.ErrNotFound)
	}

	announcement := announcementDate(ctx, e.projects, projectUID)

	created := false
	err := e.uow.Do(ctx, func(tx port.Tx) error {
		formation, createErr := tx.Formations().Create(ctx, &model.Formation{
			ProjectUID: projectUID,
			// Pinned here and never revisited. A template published later
			// cannot reshape a checklist already in flight, which is the
			// whole reason the version is copied rather than joined to.
			TemplateUID:     tpl.UID,
			TemplateVersion: tpl.Version,
			Lifecycle:       model.LifecycleLive,
			Revision:        1,
			// Copied in for the same reason Item.Title is: so a later edit to
			// the template's section titles cannot change what an existing
			// checklist displays, and so the response's sections[] always
			// covers whatever section_key an item on this checklist carries.
			Sections: sectionsSnapshot(tpl),
		})
		if createErr != nil {
			if errors.Is(createErr, domain.ErrAlreadyExists) {
				return nil
			}
			return fmt.Errorf("creating formation for %s: %w", projectUID, createErr)
		}

		items := expandItems(formation.UID, tpl, projectUID, announcement)
		// InsertMany leaves items already present by key untouched, so this
		// is safe even if a concurrent replica inserted them between the
		// Create above and here.
		// The inserted keys are not needed here: the Create above returned
		// ErrAlreadyExists to any concurrent replica, so this transaction is the
		// only one expanding this checklist and every item is its own.
		if _, insertErr := tx.Items().InsertMany(ctx, items); insertErr != nil {
			return fmt.Errorf("expanding %d items for %s: %w", len(items), projectUID, insertErr)
		}

		// The expansion is an auditable event in its own right: it is how every
		// item on the checklist came to exist, and without it the feed opens on
		// a checklist whose origin is the one thing it cannot explain.
		//
		// Formation-level, so no item UID — it is the whole checklist that was
		// created. Attributed to the system because no person asked for it; the
		// reconcile did, on the strength of the project's stage.
		if activityErr := tx.Activity().Append(ctx, &model.ActivityEntry{
			FormationUID: formation.UID,
			Actor:        actorSystem,
			SetBy:        model.SetBySystem,
			Action:       ActionTemplateExpanded,
			After: map[string]any{
				"template_uid":     tpl.UID.String(),
				"template_version": tpl.Version,
				"items":            len(items),
			},
		}); activityErr != nil {
			return fmt.Errorf("recording the expansion for %s: %w", projectUID, activityErr)
		}

		created = true
		return nil
	})
	if err != nil {
		return false, err
	}

	if created {
		slog.InfoContext(ctx, "created formation checklist",
			"project_uid", projectUID,
			"template_uid", tpl.UID,
			"template_version", tpl.Version,
		)
	}
	return created, nil
}

// announcementDate reads the project's announcement date, tolerating every way
// that read can fail. It only feeds due dates, and a checklist with no due dates
// is usable while a project with no checklist is not — so a failure here must
// not stop creation.
// Shared with the upgrade job rather than a method, so both paths resolve due
// dates from one implementation.
func announcementDate(ctx context.Context, projects port.ProjectReader, projectUID string) *time.Time {
	if projects == nil {
		return nil
	}

	settings, err := projects.GetSettings(ctx, projectUID)
	if err != nil {
		slog.WarnContext(ctx, "could not read announcement date; due dates left unset",
			"project_uid", projectUID, "error", err)
		return nil
	}
	if settings == nil || settings.AnnouncementDate == nil || *settings.AnnouncementDate == "" {
		return nil
	}

	parsed, err := time.Parse(time.DateOnly, *settings.AnnouncementDate)
	if err != nil {
		slog.WarnContext(ctx, "announcement date is not a date; due dates left unset",
			"project_uid", projectUID, "announcement_date", *settings.AnnouncementDate)
		return nil
	}
	return &parsed
}

// expandItems flattens the template into rows. Position is assigned per section
// from the template's order, which is the order the checklist screen renders.
func expandItems(
	formationUID uuid.UUID,
	tpl *model.Template,
	projectUID string,
	announcement *time.Time,
) []*model.Item {
	items := make([]*model.Item, 0, countTemplateItems(tpl))

	for _, section := range tpl.Sections {
		for position, templateItem := range section.Items {
			items = append(items,
				newItemFromTemplate(formationUID, section.Key, position, templateItem, projectUID, announcement))
		}
	}
	return items
}

// newItemFromTemplate builds one row from its definition. Both expansion and the
// upgrade job go through here: they add rows at different times and for
// different reasons, and a row added by an upgrade that defaulted differently
// from one added at creation would be a difference nobody could see.
func newItemFromTemplate(
	formationUID uuid.UUID,
	sectionKey string,
	position int,
	templateItem model.TemplateItem,
	projectUID string,
	announcement *time.Time,
) *model.Item {
	item := &model.Item{
		FormationUID: formationUID,
		ItemKey:      templateItem.Key,
		SectionKey:   sectionKey,
		Position:     position,

		Title:     templateItem.Title,
		OwnerTeam: templateItem.OwnerTeam,
		Gate:      templateItem.Gate,
		// Resolved rather than copied: the template's zero value and an
		// authored false mean opposite things, and the default is true because
		// acting on nearly every row needs Manage.
		RequiresWriter: templateItem.RequiresWriterOrDefault(),
		StatusSource:   templateItem.StatusSource,
		PlatformCheck:  templateItem.PlatformCheck,
		IsRequired:     templateItem.IsRequired,
		ChecklistType:  templateItem.ChecklistType,
		ActionLink:     resolveActionLink(templateItem.ActionLink, projectUID),
		DueDate:        resolveDueDate(templateItem.DueRule, announcement),
		SubItems:       expandSubItems(templateItem.SubItems),
	}
	// Fills status, checklist_type, status_source, revision and the empty
	// sub-item slice. Called here rather than left to each repository so the
	// Postgres and mock paths cannot disagree.
	item.ApplyInsertDefaults()
	return item
}

// sectionsSnapshot copies key and title from every section the template has,
// in template order, regardless of whether the section currently has items —
// matching what the response derived when it read this straight from the
// pinned template.
func sectionsSnapshot(tpl *model.Template) []model.FormationSection {
	out := make([]model.FormationSection, 0, len(tpl.Sections))
	for _, section := range tpl.Sections {
		out = append(out, model.FormationSection{Key: section.Key, Title: section.Title})
	}
	return out
}

func countTemplateItems(tpl *model.Template) int {
	n := 0
	for _, section := range tpl.Sections {
		n += len(section.Items)
	}
	return n
}

// expandSubItems copies the template's sub-items in. They are display detail:
// the parent's status is never derived from them, so they all start pending
// regardless of the parent.
func expandSubItems(templateSubItems []model.TemplateSubItem) []model.SubItem {
	if len(templateSubItems) == 0 {
		return []model.SubItem{}
	}
	subItems := make([]model.SubItem, 0, len(templateSubItems))
	for _, sub := range templateSubItems {
		subItems = append(subItems, model.SubItem{
			Key:    sub.Key,
			Title:  sub.Title,
			Status: model.StatusNotStarted,
		})
	}
	return subItems
}

// resolveActionLink substitutes the project placeholders once. The slug is not
// available here — only the UID is — so a slug placeholder is left in place
// rather than replaced with an empty string, which would silently produce a
// broken link instead of an obviously unresolved one.
func resolveActionLink(link, projectUID string) string {
	if link == "" {
		return ""
	}
	return strings.ReplaceAll(link, projectUIDPlaceholder, projectUID)
}

// resolveDueDate applies a due rule against the announcement date. With no rule
// or no announcement date there is no due date, which is a normal state rather
// than an error: the date is often unset when a formation starts.
func resolveDueDate(rule string, announcement *time.Time) *time.Time {
	if rule == "" || announcement == nil {
		return nil
	}
	offset, ok := ParseDueRule(rule)
	if !ok {
		// Not an error the caller can act on, and refusing to create the
		// checklist over a malformed due rule would be worse than a row with
		// no due date.
		slog.Warn("ignoring unrecognised due rule", "due_rule", rule)
		return nil
	}
	due := announcement.AddDate(0, 0, -offset)
	return &due
}

// ParseDueRule reads "announcement-30d" as thirty days before the announcement.
// Exported so the seed job can refuse an unparseable rule at load time: here it
// only warns and leaves the date unset, which ships a checklist that silently
// lost its dates.
func ParseDueRule(rule string) (int, bool) {
	if !strings.HasPrefix(rule, dueRulePrefix) {
		return 0, false
	}
	days, ok := strings.CutSuffix(strings.TrimPrefix(rule, dueRulePrefix), "d")
	if !ok {
		return 0, false
	}
	offset, err := strconv.Atoi(days)
	if err != nil || offset < 0 {
		return 0, false
	}
	return offset, true
}
