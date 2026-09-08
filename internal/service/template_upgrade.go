// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// Upgrader brings an existing checklist onto a newer template version.
//
// This is deliberately an operator job rather than a route. A checklist pins the
// template it was created from and that pin is never revisited automatically, so
// nothing here ever runs on its own — which is what stops a template edit from
// reshaping work already in flight.
type Upgrader struct {
	selector *TemplateSelector
	uow      port.UnitOfWork
	// projects supplies the announcement date due rules resolve against. It is
	// here for the same reason expansion has it: a row added by an upgrade must
	// come out identical to the same row added at creation, and a due date
	// missing from one but not the other is exactly the silent difference
	// newItemFromTemplate exists to prevent. May be nil, in which case due
	// rules resolve to no date — the same degradation creation takes.
	projects port.ProjectReader
}

// NewUpgrader wires an upgrader.
func NewUpgrader(selector *TemplateSelector, uow port.UnitOfWork, projects port.ProjectReader) *Upgrader {
	return &Upgrader{selector: selector, uow: uow, projects: projects}
}

// UpgradeReport is what one project's upgrade did, for the operator running it.
type UpgradeReport struct {
	ProjectUID string
	// TemplateUID and TemplateVersion identify the template the missing items
	// were taken from, which is not necessarily the one the checklist pins.
	TemplateUID     string
	TemplateVersion int
	// AddedKeys names the rows this run set out to add, in order. Empty means
	// the checklist was already complete against this template, which is the
	// expected outcome of a re-run.
	//
	// It is what was attempted rather than what was inserted: the insert is
	// ON CONFLICT DO NOTHING and reports no count, so two upgraders racing the
	// same project would both claim every key while only one wrote it. The rows
	// stay correct either way — UNIQUE (formation_uid, item_key) holds — so this
	// is a reporting limit, and it only shows if this is ever run in parallel.
	AddedKeys []string
}

// UpgradeFor adds the items projectUID's checklist is missing by key.
//
// It adds only. Nothing is removed and no status is touched, so an item dropped
// from the template stays on existing checklists and a row someone has already
// worked is never reset. Re-running it is a no-op.
func (u *Upgrader) UpgradeFor(ctx context.Context, projectUID string) (*UpgradeReport, error) {
	tpl, err := u.selector.Select(ctx)
	if err != nil {
		return nil, err
	}

	report := &UpgradeReport{
		ProjectUID:      projectUID,
		TemplateUID:     tpl.UID.String(),
		TemplateVersion: tpl.Version,
	}

	// Read before the transaction opens: it is a NATS round-trip once wired, and
	// holding a transaction open across it would hold the row locks with it.
	announcement := announcementDate(ctx, u.projects, projectUID)

	err = u.uow.Do(ctx, func(tx port.Tx) error {
		formation, getErr := tx.Formations().GetByProject(ctx, projectUID)
		if getErr != nil {
			return fmt.Errorf("reading formation for %s: %w", projectUID, getErr)
		}

		existing, listErr := tx.Items().ListByFormation(ctx, formation.UID)
		if listErr != nil {
			return fmt.Errorf("listing items for %s: %w", projectUID, listErr)
		}

		missing := missingItems(formation.UID, tpl, projectUID, existing, announcement)
		if len(missing) > 0 {
			// The keys come back from the insert rather than from the set
			// computed above, because those two differ under a concurrent
			// upgrade: both callers can compute the same missing set, and the
			// one that loses each row has its insert suppressed. Recording its
			// own input would put an entry in the audit trail claiming it
			// added items another transaction added, and the feed is the one
			// place that has to be literally true.
			addedKeys, insertErr := tx.Items().InsertMany(ctx, missing)
			if insertErr != nil {
				return fmt.Errorf("adding %d items to %s: %w", len(missing), projectUID, insertErr)
			}
			// Nothing landed if addedKeys is empty, so another upgrade added
			// them all first — no entry, for the same reason a fully caught-up
			// checklist writes none.
			if len(addedKeys) > 0 {
				// Recorded for the same reason the expansion is, and with more
				// force: items appearing on a checklist someone is part-way
				// through is exactly the change they will ask about, and the
				// keys are what answers it.
				if activityErr := tx.Activity().Append(ctx, &model.ActivityEntry{
					FormationUID: formation.UID,
					Actor:        actorSystem,
					SetBy:        model.SetBySystem,
					Action:       ActionTemplateUpgraded,
					After: map[string]any{
						"template_uid":     tpl.UID.String(),
						"template_version": tpl.Version,
						"added":            len(addedKeys),
						"added_keys":       addedKeys,
					},
				}); activityErr != nil {
					return fmt.Errorf("recording the upgrade for %s: %w", projectUID, activityErr)
				}
				report.AddedKeys = append(report.AddedKeys, addedKeys...)
			}
		}

		// Checked every run, not only when something was just added: a section
		// this checklist has an item for but never recorded is a gap a past
		// run may have left — its own item insert landed but its section write
		// did not — and re-checking against the actual items is what closes
		// that gap rather than only ever widening it forward.
		needed := sectionsToRecord(formation.Sections, tpl, existing, missing)
		if len(needed) > 0 {
			merged := append(append([]model.FormationSection{}, formation.Sections...), needed...)
			if _, secErr := tx.Formations().UpdateSections(ctx, formation.UID, merged, formation.Revision); secErr != nil {
				// Not fatal to this run: the items themselves are already
				// correct, this is metadata only, and the same gap is detected
				// and retried on the next run since nothing here is consumed.
				slog.WarnContext(ctx, "could not record a new section for a checklist; a later run retries",
					"project_uid", projectUID, "error", secErr)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// The pin is left alone on purpose. It records the template the checklist
	// was *created from*, which is provenance rather than a statement about
	// which items it currently holds — and after an upgrade the two genuinely
	// differ, because an item dropped from the newer template is still here.
	// Moving it would overwrite the one durable record of how the checklist
	// came to be, and nothing reads it to decide what to add: this job
	// compares keys, so it is idempotent without the pin's help.
	slog.InfoContext(ctx, "template upgrade complete",
		"project_uid", projectUID,
		"template_uid", report.TemplateUID,
		"template_version", report.TemplateVersion,
		"added", len(report.AddedKeys),
		"added_keys", report.AddedKeys,
	)
	return report, nil
}

// UpgradeAll runs the upgrade across every checklist that exists.
//
// One project's failure does not stop the sweep: an operator running this wants
// the other projects upgraded and a list of what failed, not a partial run that
// stopped at the first problem and left the rest unexplained.
func (u *Upgrader) UpgradeAll(ctx context.Context) ([]*UpgradeReport, error) {
	var projectUIDs []string
	err := u.uow.Do(ctx, func(tx port.Tx) error {
		var listErr error
		projectUIDs, listErr = tx.Formations().ListProjectUIDs(ctx)
		return listErr
	})
	if err != nil {
		return nil, fmt.Errorf("listing formations: %w", err)
	}

	reports := make([]*UpgradeReport, 0, len(projectUIDs))
	var failed int
	for _, projectUID := range projectUIDs {
		report, upgradeErr := u.UpgradeFor(ctx, projectUID)
		if upgradeErr != nil {
			failed++
			slog.ErrorContext(ctx, "upgrade failed for one project; continuing",
				"project_uid", projectUID, "error", upgradeErr)
			continue
		}
		reports = append(reports, report)
	}

	if failed > 0 {
		return reports, fmt.Errorf("%d of %d checklists failed to upgrade", failed, len(projectUIDs))
	}
	return reports, nil
}

// sectionsToRecord returns the section entries the checklist has an item for,
// across every group given, but has not recorded in have. Title is resolved
// from the template, which is why a section with no items in the current
// template (only ever true of a section only old, already-inserted items still
// reference) would come back with no title — a case this only reaches when have
// already omits a section every current item is in, which does not happen from
// this job alone.
func sectionsToRecord(
	have []model.FormationSection, tpl *model.Template, itemGroups ...[]*model.Item,
) []model.FormationSection {
	known := make(map[string]bool, len(have))
	for _, s := range have {
		known[s.Key] = true
	}
	titles := make(map[string]string, len(tpl.Sections))
	for _, s := range tpl.Sections {
		titles[s.Key] = s.Title
	}

	var out []model.FormationSection
	for _, items := range itemGroups {
		for _, item := range items {
			if known[item.SectionKey] {
				continue
			}
			known[item.SectionKey] = true
			out = append(out, model.FormationSection{Key: item.SectionKey, Title: titles[item.SectionKey]})
		}
	}
	return out
}

// missingItems returns the template's items that the checklist does not already
// have, matched on key alone. Key is the only identity an item has across
// template versions, which is what makes this comparison — and therefore the
// whole job — idempotent.
func missingItems(
	formationUID uuid.UUID,
	tpl *model.Template,
	projectUID string,
	existing []*model.Item,
	announcement *time.Time,
) []*model.Item {
	have := make(map[string]bool, len(existing))
	// Added rows go after what is already in their section rather than at the
	// template's own position, which would collide with an existing row and
	// make the rendered order depend on which of the two was read first.
	nextPosition := make(map[string]int, len(existing))
	for _, item := range existing {
		have[item.ItemKey] = true
		if item.Position >= nextPosition[item.SectionKey] {
			nextPosition[item.SectionKey] = item.Position + 1
		}
	}

	missing := make([]*model.Item, 0)
	for _, section := range tpl.Sections {
		for _, templateItem := range section.Items {
			if have[templateItem.Key] {
				continue
			}
			item := newItemFromTemplate(
				formationUID, section.Key, nextPosition[section.Key], templateItem, projectUID, announcement)
			nextPosition[section.Key]++
			missing = append(missing, item)
		}
	}
	return missing
}
