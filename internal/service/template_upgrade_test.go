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

// republish replaces the seeded template with a newer content set at the same
// priority, which is what an operator does before running the upgrade.
func republish(t *testing.T, repo *mock.TemplateRepository, version int, sections []model.TemplateSection) {
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

	republish(t, f.templates, 1, sectionsPlusOneMinusOne())

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
	republish(t, f.templates, 1, sectionsPlusOneMinusOne())

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
	republish(t, f.templates, 1, withDueRule)

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
	republish(t, f.templates, 1, sectionsPlusOneMinusOne())

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

	republish(t, f.templates, 1, sectionsPlusOneMinusOne())

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
