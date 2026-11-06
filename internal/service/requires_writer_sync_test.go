// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

const syncTemplateName = "Project formation"

type syncFixture struct {
	sync       *RequiresWriterSync
	uow        port.UnitOfWork
	templates  *mock.TemplateRepository
	formations *mock.FormationRepository
	items      *mock.ItemRepository
	// legacy is the archived version 1 that existing checklists pin.
	legacy uuid.UUID
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	templates := mock.NewTemplateRepository()
	uow := mock.NewUnitOfWork(formations, items, mock.NewActivityRepository(), templates, mock.NewApplicationRepository())
	f := &syncFixture{
		sync:       NewRequiresWriterSync(NewTemplateSelector(templates), uow),
		uow:        uow,
		templates:  templates,
		formations: formations,
		items:      items,
	}
	f.legacy = f.upsert(t, syncTemplateName, 1, model.TemplateArchived, nil)
	return f
}

func (f *syncFixture) upsert(t *testing.T, name string, version int, state model.TemplateState, sections []model.TemplateSection) uuid.UUID {
	t.Helper()
	if sections == nil {
		sections = []model.TemplateSection{}
	}
	tpl, err := f.templates.Upsert(context.Background(), &model.Template{
		Name:     name,
		Version:  version,
		State:    state,
		Priority: 100,
		Match:    MatchAlways,
		Sections: sections,
	})
	if err != nil {
		t.Fatalf("upserting %s v%d: %v", name, version, err)
	}
	return tpl.UID
}

func (f *syncFixture) publish(t *testing.T, version int, sections []model.TemplateSection) {
	t.Helper()
	f.upsert(t, syncTemplateName, version, model.TemplatePublished, sections)
}

// checklist creates one formation pinned to the legacy version, in lifecycle,
// with every key set to requiresWriter.
func (f *syncFixture) checklist(t *testing.T, project string, lifecycle model.Lifecycle, requiresWriter bool, keys ...string) uuid.UUID {
	t.Helper()
	return f.checklistFrom(t, f.legacy, project, lifecycle, requiresWriter, keys...).UID
}

func (f *syncFixture) checklistFrom(t *testing.T, templateUID uuid.UUID, project string, lifecycle model.Lifecycle, requiresWriter bool, keys ...string) *model.Formation {
	t.Helper()
	ctx := context.Background()
	formation, err := f.formations.Create(ctx, &model.Formation{
		ProjectUID:      project,
		TemplateUID:     templateUID,
		TemplateVersion: 1,
	})
	if err != nil {
		t.Fatalf("creating formation: %v", err)
	}
	if lifecycle != model.LifecycleLive {
		if formation, err = f.formations.UpdateLifecycle(ctx, formation.UID, lifecycle, formation.Revision); err != nil {
			t.Fatalf("moving formation to %s: %v", lifecycle, err)
		}
	}
	due := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	items := make([]*model.Item, 0, len(keys))
	for i, key := range keys {
		items = append(items, &model.Item{
			FormationUID:   formation.UID,
			ItemKey:        key,
			SectionKey:     "community_and_launch",
			Position:       i,
			Title:          key,
			RequiresWriter: requiresWriter,
			StatusSource:   model.SourceManual,
			Status:         model.StatusInProgress,
			Assignee:       "assignee",
			Note:           "note",
			EvidenceLink:   "https://example.org/evidence",
			DueDate:        &due,
		})
	}
	if _, err := f.items.InsertMany(ctx, items); err != nil {
		t.Fatalf("inserting items: %v", err)
	}
	return formation
}

func (f *syncFixture) byKey(t *testing.T, formationUID uuid.UUID) map[string]*model.Item {
	t.Helper()
	items, err := f.items.ListByFormation(context.Background(), formationUID)
	if err != nil {
		t.Fatalf("listing items: %v", err)
	}
	out := make(map[string]*model.Item, len(items))
	for _, item := range items {
		out[item.ItemKey] = item
	}
	return out
}

func writerSplitSections() []model.TemplateSection {
	yes, no := true, false
	return []model.TemplateSection{{
		Key:   "community_and_launch",
		Title: "Community and launch",
		Items: []model.TemplateItem{
			{Key: "mailing_lists", Title: "Mailing lists", StatusSource: model.SourceManual, RequiresWriter: &yes},
			{Key: "domain_dns", Title: "Domain and DNS", StatusSource: model.SourceManual, RequiresWriter: &no},
			{Key: "comms", Title: "Comms", StatusSource: model.SourceManual, RequiresWriter: &no},
		},
	}}
}

func resultFor(t *testing.T, report *RequiresWriterReport, key string) RequiresWriterKeyResult {
	t.Helper()
	for _, result := range report.Keys {
		if result.ItemKey == key {
			return result
		}
	}
	t.Fatalf("report has no result for %q", key)
	return RequiresWriterKeyResult{}
}

func TestRequiresWriterSyncDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.publish(t, 3, writerSplitSections())
	live := f.checklist(t, "p-live", model.LifecycleLive, true, "mailing_lists", "domain_dns", "comms")
	before := f.byKey(t, live)

	report, err := f.sync.Run(ctx, false)
	if err != nil {
		t.Fatalf("Run(dry run) = %v, want no error", err)
	}

	if report.Applied {
		t.Error("report.Applied = true for a dry run")
	}
	if report.TemplateVersion != 3 {
		t.Errorf("template version = %d, want 3", report.TemplateVersion)
	}
	if got := resultFor(t, report, "domain_dns"); got.Changed != 1 || got.Want {
		t.Errorf("domain_dns = %+v, want 1 change to false", got)
	}
	if got := resultFor(t, report, "mailing_lists"); got.Changed != 0 || got.AlreadyCorrect != 1 {
		t.Errorf("mailing_lists = %+v, want already correct", got)
	}
	if report.Changed() != 2 {
		t.Errorf("changed = %d, want 2", report.Changed())
	}

	after := f.byKey(t, live)
	for key, item := range after {
		if item.RequiresWriter != before[key].RequiresWriter {
			t.Errorf("dry run changed %s requires_writer", key)
		}
	}
}

func TestRequiresWriterSyncAppliesOnlyToLiveMismatches(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.publish(t, 3, writerSplitSections())
	live := f.checklist(t, "p-live", model.LifecycleLive, true, "mailing_lists", "domain_dns", "comms")
	completed := f.checklist(t, "p-done", model.LifecycleCompleted, true, "mailing_lists", "domain_dns", "comms")
	frozen := f.checklist(t, "p-frozen", model.LifecycleFrozen, true, "mailing_lists", "domain_dns", "comms")
	before := f.byKey(t, live)

	report, err := f.sync.Run(ctx, true)
	if err != nil {
		t.Fatalf("Run(apply) = %v, want no error", err)
	}
	if !report.Applied {
		t.Error("report.Applied = false after an apply")
	}
	if report.Changed() != 2 {
		t.Errorf("changed = %d, want 2 — only the live checklist's two false rows", report.Changed())
	}
	if report.Finished != 6 {
		t.Errorf("finished = %d, want 6 — three items on each of the completed and frozen checklists", report.Finished)
	}

	after := f.byKey(t, live)
	want := map[string]bool{"mailing_lists": true, "domain_dns": false, "comms": false}
	for key, item := range after {
		if item.RequiresWriter != want[key] {
			t.Errorf("live %s requires_writer = %v, want %v", key, item.RequiresWriter, want[key])
		}
		// Everything a person may have set stays as it was. The revision in
		// particular: bumping it would fail a concurrent editor's precondition
		// for a change they cannot see.
		b := before[key]
		if item.Status != b.Status || item.StatusSource != b.StatusSource ||
			item.Assignee != b.Assignee || item.Note != b.Note ||
			item.EvidenceLink != b.EvidenceLink || !item.DueDate.Equal(*b.DueDate) ||
			item.Revision != b.Revision || !item.UpdatedAt.Equal(b.UpdatedAt) {
			t.Errorf("live %s changed beyond requires_writer:\n before %+v\n after  %+v", key, b, item)
		}
	}

	for name, uid := range map[string]uuid.UUID{"completed": completed, "frozen": frozen} {
		for key, item := range f.byKey(t, uid) {
			if !item.RequiresWriter {
				t.Errorf("%s checklist %s requires_writer changed to false; finished checklists are left alone", name, key)
			}
		}
	}
}

func TestRequiresWriterSyncIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.publish(t, 3, writerSplitSections())
	f.checklist(t, "p-live", model.LifecycleLive, true, "mailing_lists", "domain_dns", "comms")

	if _, err := f.sync.Run(ctx, true); err != nil {
		t.Fatalf("first apply = %v", err)
	}
	second, err := f.sync.Run(ctx, true)
	if err != nil {
		t.Fatalf("second apply = %v", err)
	}
	if second.Changed() != 0 {
		t.Errorf("second apply changed %d items, want 0", second.Changed())
	}
	if second.AlreadyCorrect() != 3 {
		t.Errorf("second apply already correct = %d, want 3", second.AlreadyCorrect())
	}
}

// lifecycleFlipUoW moves a checklist between the count's transaction and the
// apply's, which is the window a real run leaves open.
type lifecycleFlipUoW struct {
	port.UnitOfWork
	calls  int
	before func()
}

func (u *lifecycleFlipUoW) Do(ctx context.Context, fn func(port.Tx) error) error {
	u.calls++
	if u.calls == 2 && u.before != nil {
		u.before()
	}
	return u.UnitOfWork.Do(ctx, fn)
}

// The count is a preflight in its own transaction. A checklist unfrozen after
// it — a project moving from Disengaged back to a forming stage — holds keys
// the count saw no live mismatch for, and the apply still has to reach them.
func TestRequiresWriterSyncCorrectsAChecklistThatWentLiveAfterTheCount(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.publish(t, 3, writerSplitSections())
	frozen := f.checklistFrom(t, f.legacy, "p-unfrozen", model.LifecycleFrozen, true, "mailing_lists", "domain_dns", "comms")

	uow := &lifecycleFlipUoW{UnitOfWork: f.uow, before: func() {
		if _, err := f.formations.UpdateLifecycle(ctx, frozen.UID, model.LifecycleLive, frozen.Revision); err != nil {
			t.Fatalf("unfreezing: %v", err)
		}
	}}
	report, err := NewRequiresWriterSync(NewTemplateSelector(f.templates), uow).Run(ctx, true)
	if err != nil {
		t.Fatalf("Run(apply) = %v", err)
	}

	want := map[string]bool{"mailing_lists": true, "domain_dns": false, "comms": false}
	for key, item := range f.byKey(t, frozen.UID) {
		if item.RequiresWriter != want[key] {
			t.Errorf("%s requires_writer = %v, want %v — the apply skipped a key the stale count found nothing wrong with", key, item.RequiresWriter, want[key])
		}
	}
	if report.Changed() != 2 || report.AlreadyCorrect() != 1 || report.Finished != 0 {
		t.Errorf("changed = %d, already correct = %d, finished = %d; want 2, 1 and 0 — the report should describe the checklist as the apply found it",
			report.Changed(), report.AlreadyCorrect(), report.Finished)
	}
}

// Another template's checklists may share item keys and choose differently.
// The sync reads and writes only the selected template's family.
func TestRequiresWriterSyncLeavesAnotherTemplatesChecklistsAlone(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.publish(t, 3, writerSplitSections())
	other := f.upsert(t, "Other formation", 1, model.TemplateArchived, nil)
	mine := f.checklist(t, "p-mine", model.LifecycleLive, true, "mailing_lists", "domain_dns", "comms")
	theirs := f.checklistFrom(t, other, "p-theirs", model.LifecycleLive, true, "mailing_lists", "domain_dns", "comms").UID

	report, err := f.sync.Run(ctx, true)
	if err != nil {
		t.Fatalf("Run(apply) = %v", err)
	}
	if report.Changed() != 2 || report.AlreadyCorrect() != 1 {
		t.Errorf("changed = %d, already correct = %d; want 2 and 1 — this family's checklist only", report.Changed(), report.AlreadyCorrect())
	}
	if f.byKey(t, mine)["domain_dns"].RequiresWriter {
		t.Error("this family's domain_dns was not corrected")
	}
	for key, item := range f.byKey(t, theirs) {
		if !item.RequiresWriter {
			t.Errorf("another template's %s was rewritten", key)
		}
	}
}

// A key the selected template does not carry has no value to sync to. Leaving
// it alone and naming it is the only honest outcome; guessing the default would
// rewrite a row nobody decided about.
func TestRequiresWriterSyncSkipsKeysNotInTheTemplate(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.publish(t, 3, writerSplitSections())
	live := f.checklist(t, "p-live", model.LifecycleLive, true, "mailing_lists", "retired_row")

	report, err := f.sync.Run(ctx, true)
	if err != nil {
		t.Fatalf("Run(apply) = %v", err)
	}
	if len(report.Skipped) != 1 || report.Skipped[0].ItemKey != "retired_row" || report.Skipped[0].Count != 1 {
		t.Fatalf("skipped = %+v, want one retired_row", report.Skipped)
	}
	if report.Skipped[0].Reason == "" {
		t.Error("skipped entry carries no reason")
	}
	if !f.byKey(t, live)["retired_row"].RequiresWriter {
		t.Error("retired_row was rewritten; a key outside the template must be left alone")
	}
}

func TestRequiresWriterSyncRefusesWithoutATemplate(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	live := f.checklist(t, "p-live", model.LifecycleLive, true, "mailing_lists")

	if _, err := f.sync.Run(ctx, true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Run with no published template = %v, want domain.ErrNotFound", err)
	}
	if !f.byKey(t, live)["mailing_lists"].RequiresWriter {
		t.Error("a refused run wrote anyway")
	}
}

// Run before the corrected template is published, the sync would select the
// one that leaves the flag unset, find every live item already holding the
// true default, and report success.
func TestRequiresWriterSyncRefusesATemplateThatLeavesTheFlagUnset(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	sections := writerSplitSections()
	sections[0].Items[1].RequiresWriter = nil
	f.publish(t, 2, sections)
	f.checklist(t, "p-live", model.LifecycleLive, true, "mailing_lists", "domain_dns", "comms")

	if _, err := f.sync.Run(ctx, true); err == nil {
		t.Fatal("Run against a template leaving requires_writer unset = nil, want a refusal")
	}
	if n := f.items.Calls()["items.CountRequiresWriter"]; n != 0 {
		t.Errorf("a refused run read items %d times", n)
	}
	if n := f.items.Calls()["items.SetLiveRequiresWriter"]; n != 0 {
		t.Errorf("a refused run reached the write %d times", n)
	}
}

func TestRequiresWriterSyncRefusesAnEmptyTemplate(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.publish(t, 3, []model.TemplateSection{})
	f.checklist(t, "p-live", model.LifecycleLive, true, "mailing_lists")

	if _, err := f.sync.Run(ctx, true); err == nil {
		t.Fatal("Run against a template with no items = nil, want a refusal")
	}
	if n := f.items.Calls()["items.SetLiveRequiresWriter"]; n != 0 {
		t.Errorf("a refused run reached the write %d times", n)
	}
}
