// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"sort"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// skipReasonNotInTemplate is reported for a live item whose key the selected
// template does not carry, so there is no value to correct it to.
const skipReasonNotInTemplate = "not in selected template"

// RequiresWriterSync sets requires_writer on live checklists' items to the
// value the selected published template gives each item's key.
//
// An operator job, like Upgrader, and not part of the reconcile. Expansion
// already copies the flag, so a checklist created from the current template is
// right without this. What this corrects, once, is checklists created before
// the template stated the flag, which pin a version whose unset flag resolved
// to true everywhere; it deliberately applies the current template's values to
// them regardless of the version they pin. In the sweep it would instead keep
// overriding the expansion-time copy on every pass, whenever a later template
// changed a value.
type RequiresWriterSync struct {
	selector *TemplateSelector
	uow      port.UnitOfWork
}

// NewRequiresWriterSync wires the correction.
func NewRequiresWriterSync(selector *TemplateSelector, uow port.UnitOfWork) *RequiresWriterSync {
	return &RequiresWriterSync{selector: selector, uow: uow}
}

// RequiresWriterReport is what one run found and, when applied, changed.
type RequiresWriterReport struct {
	TemplateUID     string
	TemplateName    string
	TemplateVersion int
	Applied         bool
	// Keys follows template order. Changed counts the live items holding the
	// opposite of Want: on a dry run the ones that would change, on an apply
	// the ones the write actually changed.
	Keys []RequiresWriterKeyResult
	// Skipped names the keys live items carry that the template does not.
	Skipped []RequiresWriterSkip
}

// RequiresWriterKeyResult is one template key's outcome.
type RequiresWriterKeyResult struct {
	ItemKey        string
	Want           bool
	Changed        int
	AlreadyCorrect int
}

// RequiresWriterSkip is one key left alone, and why.
type RequiresWriterSkip struct {
	ItemKey string
	Count   int
	Reason  string
}

// Changed totals the items changed, or that would change on a dry run.
func (r *RequiresWriterReport) Changed() int {
	n := 0
	for _, k := range r.Keys {
		n += k.Changed
	}
	return n
}

// AlreadyCorrect totals the items that already held the template's value.
func (r *RequiresWriterReport) AlreadyCorrect() int {
	n := 0
	for _, k := range r.Keys {
		n += k.AlreadyCorrect
	}
	return n
}

// Run compares live items with the selected template and, when apply is set,
// corrects the ones that differ in a single transaction. A dry run writes
// nothing. It refuses before reading any item when no template is selected,
// the selected one has no items, or it leaves the flag unset on any item.
func (s *RequiresWriterSync) Run(ctx context.Context, apply bool) (*RequiresWriterReport, error) {
	tpl, err := s.selector.Select(ctx)
	if err != nil {
		return nil, fmt.Errorf("selecting the template to sync against: %w", err)
	}

	report := &RequiresWriterReport{
		TemplateUID:     tpl.UID.String(),
		TemplateName:    tpl.Name,
		TemplateVersion: tpl.Version,
		Applied:         apply,
	}
	want := make(map[string]bool)
	unstated := 0
	for _, section := range tpl.Sections {
		for _, item := range section.Items {
			if item.RequiresWriter == nil {
				unstated++
			}
			want[item.Key] = item.RequiresWriterOrDefault()
			report.Keys = append(report.Keys, RequiresWriterKeyResult{
				ItemKey: item.Key,
				Want:    item.RequiresWriterOrDefault(),
			})
		}
	}
	if len(report.Keys) == 0 {
		return nil, fmt.Errorf("selected template %q v%d has no items to sync against", tpl.Name, tpl.Version)
	}
	// A template that leaves the flag unset predates the correction: syncing to
	// it would find live items already holding the true default and report
	// success while changing nothing, which reads as the correction having run.
	if unstated > 0 {
		return nil, fmt.Errorf("selected template %q v%d leaves requires_writer unset on %d items; publish a template that states it on every item first",
			tpl.Name, tpl.Version, unstated)
	}

	var counts []port.RequiresWriterCount
	if err := s.uow.Do(ctx, func(tx port.Tx) error {
		var countErr error
		counts, countErr = tx.Items().CountLiveRequiresWriter(ctx)
		return countErr
	}); err != nil {
		return nil, fmt.Errorf("counting live items: %w", err)
	}

	byKey := make(map[string]map[bool]int)
	for _, c := range counts {
		if byKey[c.ItemKey] == nil {
			byKey[c.ItemKey] = make(map[bool]int)
		}
		byKey[c.ItemKey][c.RequiresWriter] += c.Count
	}
	for i := range report.Keys {
		k := &report.Keys[i]
		k.Changed = byKey[k.ItemKey][!k.Want]
		k.AlreadyCorrect = byKey[k.ItemKey][k.Want]
	}
	for key, values := range byKey {
		if _, inTemplate := want[key]; !inTemplate {
			report.Skipped = append(report.Skipped, RequiresWriterSkip{
				ItemKey: key,
				Count:   values[true] + values[false],
				Reason:  skipReasonNotInTemplate,
			})
		}
	}
	sort.Slice(report.Skipped, func(i, j int) bool { return report.Skipped[i].ItemKey < report.Skipped[j].ItemKey })

	if !apply {
		return report, nil
	}
	if err := s.uow.Do(ctx, func(tx port.Tx) error {
		for i := range report.Keys {
			k := &report.Keys[i]
			if k.Changed == 0 {
				continue
			}
			changed, setErr := tx.Items().SetLiveRequiresWriter(ctx, k.ItemKey, k.Want)
			if setErr != nil {
				return setErr
			}
			k.Changed = int(changed)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("applying requires_writer: %w", err)
	}
	return report, nil
}
