// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// publishVersion publishes a further template version at the same priority,
// which is what an operator does before running the upgrade.
//
// It has to be a new version rather than an edit of the seeded one: a published
// version's content is immutable, because checklists pin the version they
// expanded from. That leaves two published versions at one priority, so these
// tests also depend on selection preferring the newer one.
func publishNextVersion(t *testing.T, repo *mock.TemplateRepository, version int, sections []model.TemplateSection) {
	t.Helper()
	_, err := repo.Upsert(context.Background(), &model.Template{
		Name:     "Project formation",
		Version:  version,
		State:    model.TemplatePublished,
		Priority: 100,
		Match:    MatchAlways,
		Sections: sections,
	})
	if err != nil {
		t.Fatalf("Upsert(v%d) = %v, want no error", version, err)
	}
}

// sectionsPlusOne is twoItemSections with one extra row in the launch section
// and one row removed from the legal section, which is the interesting shape:
// the upgrade must add the new row and must NOT remove the dropped one.
func sectionsPlusOneMinusOne() []model.TemplateSection {
	return []model.TemplateSection{
		{
			Key:   "legal_and_entity",
			Title: "Legal and entity",
			Items: []model.TemplateItem{
				{
					Key:          "charter_agreed",
					Title:        "Charter agreed",
					OwnerTeam:    "formation",
					Gate:         true,
					StatusSource: model.SourceManual,
				},
				// membership_tiers is deliberately absent.
			},
		},
		{
			Key:   "community_and_launch",
			Title: "Community and launch",
			Items: []model.TemplateItem{
				{
					Key:           "mailing_lists",
					Title:         "Mailing lists",
					OwnerTeam:     "community",
					StatusSource:  model.SourcePlatform,
					PlatformCheck: &model.PlatformCheck{ResourceType: "mailing_list", MinCount: 1},
				},
				{
					Key:          "blog_post",
					Title:        "Launch blog post",
					OwnerTeam:    "marketing",
					StatusSource: model.SourceManual,
				},
			},
		},
	}
}

func TestUpgradeAddsOnlyMissingItems(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), nil)

	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("ExpandFor() = %v, want no error", err)
	}
	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}

	// Work one row and skip another, so a reset would be visible.
	done := model.StatusDone
	worked, err := f.items.GetByKey(ctx, formation.UID, "charter_agreed")
	if err != nil {
		t.Fatalf("GetByKey() = %v, want no error", err)
	}
	if _, err = f.items.Update(ctx, worked.UID, worked.Revision, port.ItemPatch{Status: &done}); err != nil {
		t.Fatalf("Update() = %v, want no error", err)
	}

	publishNextVersion(t, f.templates, 2, sectionsPlusOneMinusOne())

	upgrader := NewUpgrader(NewTemplateSelector(f.templates), f.uow, nil)
	report, err := upgrader.UpgradeFor(ctx, "project-1")
	if err != nil {
		t.Fatalf("UpgradeFor() = %v, want no error", err)
	}

	t.Run("only the new key is added", func(t *testing.T) {
		if len(report.AddedKeys) != 1 || report.AddedKeys[0] != "blog_post" {
			t.Errorf("added = %v, want [blog_post]", report.AddedKeys)
		}
	})

	items, err := f.items.ListByFormation(ctx, formation.UID)
	if err != nil {
		t.Fatalf("ListByFormation() = %v, want no error", err)
	}

	// An item dropped from the template stays. Removing it would delete work
	// somebody may already have done and recorded against it.
	t.Run("a row dropped from the template is left in place", func(t *testing.T) {
		if _, err := f.items.GetByKey(ctx, formation.UID, "membership_tiers"); err != nil {
			t.Errorf("membership_tiers = %v, want it still present", err)
		}
		if len(items) != 4 {
			t.Errorf("items = %d, want 4 (3 original + 1 added)", len(items))
		}
	})

	t.Run("a worked status is not reset", func(t *testing.T) {
		after, getErr := f.items.GetByKey(ctx, formation.UID, "charter_agreed")
		if getErr != nil {
			t.Fatalf("GetByKey() = %v, want no error", getErr)
		}
		if after.Status != model.StatusDone {
			t.Errorf("status = %q, want %q", after.Status, model.StatusDone)
		}
		if after.Revision != worked.Revision+1 {
			t.Errorf("revision = %d, want %d — the upgrade touched the row",
				after.Revision, worked.Revision+1)
		}
	})

	// Appended after what is already in the section, not at the template's own
	// position, which would collide with mailing_lists at 0.
	t.Run("the added row goes after the section's existing rows", func(t *testing.T) {
		added, getErr := f.items.GetByKey(ctx, formation.UID, "blog_post")
		if getErr != nil {
			t.Fatalf("GetByKey() = %v, want no error", getErr)
		}
		if added.Position != 1 {
			t.Errorf("position = %d, want 1", added.Position)
		}
		if added.SectionKey != "community_and_launch" {
			t.Errorf("section_key = %q, want community_and_launch", added.SectionKey)
		}
	})

	// The pin records what the checklist was created from. After an upgrade the
	// two genuinely differ, and overwriting it would lose the provenance.
	t.Run("the creation pin is left alone", func(t *testing.T) {
		after, getErr := f.formations.GetByProject(ctx, "project-1")
		if getErr != nil {
			t.Fatalf("GetByProject() = %v, want no error", getErr)
		}
		if after.TemplateVersion != formation.TemplateVersion {
			t.Errorf("template_version = %d, want %d", after.TemplateVersion, formation.TemplateVersion)
		}
		if after.TemplateUID != formation.TemplateUID {
			t.Errorf("template_uid = %v, want %v", after.TemplateUID, formation.TemplateUID)
		}
	})
}

// Re-running has to add nothing. An operator will run this more than once, and
// on a schedule if it is ever wired to one.
func TestUpgradeIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), nil)
	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("ExpandFor() = %v, want no error", err)
	}
	publishNextVersion(t, f.templates, 2, sectionsPlusOneMinusOne())

	upgrader := NewUpgrader(NewTemplateSelector(f.templates), f.uow, nil)
	first, err := upgrader.UpgradeFor(ctx, "project-1")
	if err != nil {
		t.Fatalf("first UpgradeFor() = %v, want no error", err)
	}
	if len(first.AddedKeys) != 1 {
		t.Fatalf("first run added %v, want one key", first.AddedKeys)
	}

	for i := range 3 {
		again, upErr := upgrader.UpgradeFor(ctx, "project-1")
		if upErr != nil {
			t.Fatalf("re-run %d = %v, want no error", i+1, upErr)
		}
		if len(again.AddedKeys) != 0 {
			t.Errorf("re-run %d added %v, want nothing", i+1, again.AddedKeys)
		}
	}

	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	items, err := f.items.ListByFormation(ctx, formation.UID)
	if err != nil {
		t.Fatalf("ListByFormation() = %v, want no error", err)
	}
	if len(items) != 4 {
		t.Errorf("items = %d, want 4", len(items))
	}
}

// A row added by an upgrade must come out the same as the same row added at
// creation. Due dates are where that is easiest to get wrong, because the
// upgrade has to make the announcement-date read for itself.
func TestUpgradeResolvesDueDatesOnAddedItems(t *testing.T) {
	ctx := context.Background()
	const announcement = "2026-04-01"
	projects := stubProjects{announcement: announcement}
	f := newExpansionFixture(t, twoItemSections(), projects)

	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("ExpandFor() = %v, want no error", err)
	}

	withDueRule := sectionsPlusOneMinusOne()
	withDueRule[1].Items[1].DueRule = "announcement-30d"
	publishNextVersion(t, f.templates, 2, withDueRule)

	upgrader := NewUpgrader(NewTemplateSelector(f.templates), f.uow, projects)
	if _, err := upgrader.UpgradeFor(ctx, "project-1"); err != nil {
		t.Fatalf("UpgradeFor() = %v, want no error", err)
	}

	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	added, err := f.items.GetByKey(ctx, formation.UID, "blog_post")
	if err != nil {
		t.Fatalf("GetByKey() = %v, want no error", err)
	}
	if added.DueDate == nil {
		t.Fatal("added item has no due date; the upgrade did not resolve its due rule")
	}
	if got := added.DueDate.Format(time.DateOnly); got != "2026-03-02" {
		t.Errorf("due date = %s, want 2026-03-02 (30 days before %s)", got, announcement)
	}
}

func TestUpgradeForAProjectWithNoChecklist(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), nil)

	upgrader := NewUpgrader(NewTemplateSelector(f.templates), f.uow, nil)
	if _, err := upgrader.UpgradeFor(ctx, "project-none"); err == nil {
		t.Error("UpgradeFor() = nil, want an error for a project with no checklist")
	}
}

// One project failing must not abandon the rest: an operator wants the others
// upgraded and a report of what failed.
func TestUpgradeAllCoversEveryChecklist(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), nil)
	for _, projectUID := range []string{"project-1", "project-2", "project-3"} {
		if _, err := f.expander.ExpandFor(ctx, projectUID); err != nil {
			t.Fatalf("ExpandFor(%s) = %v, want no error", projectUID, err)
		}
	}
	publishNextVersion(t, f.templates, 2, sectionsPlusOneMinusOne())

	reports, err := NewUpgrader(NewTemplateSelector(f.templates), f.uow, nil).UpgradeAll(ctx)
	if err != nil {
		t.Fatalf("UpgradeAll() = %v, want no error", err)
	}
	if len(reports) != 3 {
		t.Fatalf("reports = %d, want 3", len(reports))
	}
	for _, report := range reports {
		if len(report.AddedKeys) != 1 || report.AddedKeys[0] != "blog_post" {
			t.Errorf("%s added %v, want [blog_post]", report.ProjectUID, report.AddedKeys)
		}
	}
}

// The spec requires an activity entry for a template upgrade as well as an
// expansion. Items appearing on a checklist someone is part-way through is
// precisely the change they will ask about, and the keys are the answer.
func TestUpgradeRecordsWhatItAdded(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), nil)

	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("ExpandFor() = %v, want no error", err)
	}
	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v", err)
	}

	publishNextVersion(t, f.templates, 2, sectionsPlusOneMinusOne())

	u := NewUpgrader(NewTemplateSelector(f.templates), f.uow, nil)
	report, err := u.UpgradeFor(ctx, "project-1")
	if err != nil {
		t.Fatalf("UpgradeFor() = %v, want no error", err)
	}
	if len(report.AddedKeys) != 1 {
		t.Fatalf("added keys = %v, want one", report.AddedKeys)
	}

	entries, _, err := f.activity.List(ctx, formation.UID, "", 10)
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	// Newest first: the upgrade, then the expansion that preceded it.
	if len(entries) != 2 {
		t.Fatalf("activity entries = %d, want 2", len(entries))
	}
	if entries[0].Action != ActionTemplateUpgraded {
		t.Errorf("action = %q, want %q", entries[0].Action, ActionTemplateUpgraded)
	}
	keys, ok := entries[0].After["added_keys"].([]string)
	if !ok || len(keys) != 1 || keys[0] != report.AddedKeys[0] {
		t.Errorf("after[added_keys] = %v, want %v", entries[0].After["added_keys"], report.AddedKeys)
	}
}

// raceLosingItems reports that every insert was suppressed, which is what the
// repository returns when a concurrent upgrade inserted the same rows first.
type raceLosingItems struct{ port.ItemRepository }

func (raceLosingItems) InsertMany(context.Context, []*model.Item) ([]string, error) {
	return nil, nil
}

// suppressedInsertTx swaps in the item repository above, leaving the rest of the
// transaction as it was.
type suppressedInsertTx struct {
	port.Tx
	items port.ItemRepository
}

func (s suppressedInsertTx) Items() port.ItemRepository { return s.items }

type suppressedInsertUOW struct{ inner port.UnitOfWork }

func (u suppressedInsertUOW) Do(ctx context.Context, fn func(port.Tx) error) error {
	return u.inner.Do(ctx, func(tx port.Tx) error {
		return fn(suppressedInsertTx{Tx: tx, items: raceLosingItems{tx.Items()}})
	})
}

// Two upgrades running at once compute the same set of missing items, and the
// one that loses has every insert suppressed by the uniqueness constraint. It
// must not then write an activity entry claiming it added them: the feed is the
// audit trail, so an entry naming rows this transaction did not write is a
// record of something that did not happen, and both upgrades would report having
// added the same items.
func TestAnUpgradeThatLosesTheRaceRecordsNothing(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), nil)

	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("ExpandFor() = %v, want no error", err)
	}
	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v", err)
	}

	publishNextVersion(t, f.templates, 2, sectionsPlusOneMinusOne())

	u := NewUpgrader(NewTemplateSelector(f.templates), suppressedInsertUOW{f.uow}, nil)
	report, err := u.UpgradeFor(ctx, "project-1")
	if err != nil {
		t.Fatalf("UpgradeFor() = %v, want no error — losing the race is not a failure", err)
	}
	if len(report.AddedKeys) != 0 {
		t.Errorf("added keys = %v, want none — the other upgrade added them", report.AddedKeys)
	}

	entries, _, err := f.activity.List(ctx, formation.UID, "", 10)
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	// Only the expansion, which ran before this.
	if len(entries) != 1 {
		t.Fatalf("activity entries = %d, want 1 — no entry for an upgrade that added nothing", len(entries))
	}
	if entries[0].Action == ActionTemplateUpgraded {
		t.Error("an upgrade that inserted nothing recorded itself as having upgraded")
	}
}

// A version that introduces a whole new section, not just a new item in an
// existing one, is the case the checklist's section snapshot exists for: the
// upgrade adds an item whose section_key the pinned template never had, and
// the checklist's own record of its sections must grow to cover it — nothing
// downstream should ever see an item pointing at a section absent from
// sections[].
func TestUpgradeThatAddsANewSectionRecordsItOnTheChecklist(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), nil)

	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("ExpandFor() = %v, want no error", err)
	}
	before, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	if len(before.Sections) != 2 {
		t.Fatalf("sections before upgrade = %d, want 2 (from the v1 template)", len(before.Sections))
	}

	withNewSection := append(twoItemSections(), model.TemplateSection{
		Key:   "brand_review",
		Title: "Brand review",
		Items: []model.TemplateItem{
			{Key: "logo_approved", Title: "Logo approved", StatusSource: model.SourceManual},
		},
	})
	publishNextVersion(t, f.templates, 2, withNewSection)

	upgrader := NewUpgrader(NewTemplateSelector(f.templates), f.uow, nil)
	report, err := upgrader.UpgradeFor(ctx, "project-1")
	if err != nil {
		t.Fatalf("UpgradeFor() = %v, want no error", err)
	}
	if len(report.AddedKeys) != 1 || report.AddedKeys[0] != "logo_approved" {
		t.Fatalf("added = %v, want [logo_approved]", report.AddedKeys)
	}

	after, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	if len(after.Sections) != 3 {
		t.Fatalf("sections after upgrade = %d, want 3 (2 original + brand_review)", len(after.Sections))
	}
	var got *model.FormationSection
	for i := range after.Sections {
		if after.Sections[i].Key == "brand_review" {
			got = &after.Sections[i]
		}
	}
	if got == nil {
		t.Fatal("brand_review is not in the checklist's sections snapshot; the added item's section_key matches nothing in sections[]")
	}
	if got.Title != "Brand review" {
		t.Errorf("title = %q, want %q", got.Title, "Brand review")
	}

	// The two original sections are untouched — this only ever grows.
	for _, key := range []string{"legal_and_entity", "community_and_launch"} {
		found := false
		for _, s := range after.Sections {
			if s.Key == key {
				found = true
			}
		}
		if !found {
			t.Errorf("original section %q is missing after the upgrade", key)
		}
	}
}

// An upgrade that finds nothing to add must leave no trace: the feed records
// changes to the checklist, not the fact that an operator ran a job.
func TestUpgradeWithNothingToAddRecordsNothing(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), nil)

	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("ExpandFor() = %v, want no error", err)
	}
	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v", err)
	}

	u := NewUpgrader(NewTemplateSelector(f.templates), f.uow, nil)
	if _, err := u.UpgradeFor(ctx, "project-1"); err != nil {
		t.Fatalf("UpgradeFor() = %v, want no error", err)
	}

	entries, _, err := f.activity.List(ctx, formation.UID, "", 10)
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("activity entries = %d, want 1 — only the expansion", len(entries))
	}
}
