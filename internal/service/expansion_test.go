// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// expansionFixture wires an expander over mock repositories sharing one unit of
// work, which is the wiring mock mode uses in production: a fresh set of
// repositories inside Do would make writes invisible to reads outside it.
type expansionFixture struct {
	expander   *Expander
	uow        port.UnitOfWork
	formations *mock.FormationRepository
	items      *mock.ItemRepository
	templates  *mock.TemplateRepository
	activity   *mock.ActivityRepository
}

func newExpansionFixture(t *testing.T, sections []model.TemplateSection, projects port.ProjectReader) *expansionFixture {
	t.Helper()

	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	activity := mock.NewActivityRepository()
	templates := mock.NewTemplateRepository()
	uow := mock.NewUnitOfWork(formations, items, activity, templates)

	_, err := templates.Upsert(context.Background(), &model.Template{
		Name:     "Project formation",
		Version:  1,
		State:    model.TemplatePublished,
		Priority: 100,
		Match:    MatchAlways,
		Sections: sections,
	})
	if err != nil {
		t.Fatalf("seeding template = %v, want no error", err)
	}

	return &expansionFixture{
		expander:   NewExpander(NewTemplateSelector(templates), uow, projects),
		uow:        uow,
		formations: formations,
		items:      items,
		templates:  templates,
		activity:   activity,
	}
}

// The spec requires an activity entry for template expansion, committed with the
// change it records. Without it the feed opens on a checklist that cannot
// explain where any of its items came from.
func TestExpansionRecordsItselfInTheActivityFeed(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), &stubProjects{})

	created, err := f.expander.ExpandFor(ctx, "project-1")
	if err != nil {
		t.Fatalf("ExpandFor() = %v, want no error", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}

	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v", err)
	}
	entries, _, err := f.activity.List(ctx, formation.UID, "", 10)
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("activity entries = %d, want 1", len(entries))
	}

	entry := entries[0]
	if entry.Action != ActionTemplateExpanded {
		t.Errorf("action = %q, want %q", entry.Action, ActionTemplateExpanded)
	}
	if entry.SetBy != model.SetBySystem {
		t.Errorf("set_by = %q, want system — no person asked for this", entry.SetBy)
	}
	// Formation-level: it is the whole checklist that came into being, not an
	// item within it.
	if entry.ItemUID != nil {
		t.Errorf("item_uid = %v, want nil", entry.ItemUID)
	}
	if entry.After["items"] != 3 {
		t.Errorf("after[items] = %v, want 3", entry.After["items"])
	}
}

// A second expansion is absorbed by the uniqueness constraint and creates
// nothing, so it must not leave a second entry claiming otherwise.
func TestASecondExpansionRecordsNothing(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), &stubProjects{})

	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("first ExpandFor() = %v", err)
	}
	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("second ExpandFor() = %v", err)
	}

	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v", err)
	}
	entries, _, err := f.activity.List(ctx, formation.UID, "", 10)
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("activity entries = %d, want 1 — the second expansion created nothing", len(entries))
	}
}

func twoItemSections() []model.TemplateSection {
	explicitlyNoWriter := false
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
					ActionLink:   "/project/{{project.uid}}/charter",
					DueRule:      "announcement-30d",
				},
				{
					Key:            "membership_tiers",
					Title:          "Membership tiers",
					OwnerTeam:      "product",
					StatusSource:   model.SourceManual,
					RequiresWriter: &explicitlyNoWriter,
					SubItems: []model.TemplateSubItem{
						{Key: "tier_draft", Title: "Draft tiers"},
					},
				},
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
			},
		},
	}
}

// stubProjects returns a fixed announcement date, which is all expansion reads.
type stubProjects struct {
	announcement string
	err          error
}

func (s stubProjects) GetSettings(_ context.Context, projectUID string) (*port.ProjectSettings, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &port.ProjectSettings{ProjectUID: projectUID, AnnouncementDate: &s.announcement}, nil
}

func (s stubProjects) ListFormingProjects(_ context.Context, _ []string) ([]port.ProjectRef, error) {
	return nil, nil
}

func TestExpandForCreatesTheChecklist(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), stubProjects{announcement: "2026-12-01"})

	created, err := f.expander.ExpandFor(ctx, "project-1")
	if err != nil {
		t.Fatalf("ExpandFor() = %v, want no error", err)
	}
	if !created {
		t.Fatal("ExpandFor() reported no creation on an empty store")
	}

	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}

	// The pin is the point: a template published later must not reshape a
	// checklist already in flight, so the version travels onto the row.
	t.Run("template identity and version are pinned", func(t *testing.T) {
		published, listErr := f.templates.ListPublished(ctx)
		if listErr != nil {
			t.Fatalf("ListPublished() = %v, want no error", listErr)
		}
		if formation.TemplateUID != published[0].UID {
			t.Errorf("template_uid = %v, want %v", formation.TemplateUID, published[0].UID)
		}
		if formation.TemplateVersion != 1 {
			t.Errorf("template_version = %d, want 1", formation.TemplateVersion)
		}
	})

	items, err := f.items.ListByFormation(ctx, formation.UID)
	if err != nil {
		t.Fatalf("ListByFormation() = %v, want no error", err)
	}
	if len(items) != 3 {
		t.Fatalf("expanded %d items, want 3", len(items))
	}

	byKey := make(map[string]*model.Item, len(items))
	for _, item := range items {
		byKey[item.ItemKey] = item
	}

	t.Run("position is per section, from template order", func(t *testing.T) {
		if got := byKey["charter_agreed"].Position; got != 0 {
			t.Errorf("charter_agreed position = %d, want 0", got)
		}
		if got := byKey["membership_tiers"].Position; got != 1 {
			t.Errorf("membership_tiers position = %d, want 1", got)
		}
		// Restarts in the next section rather than continuing to 2.
		if got := byKey["mailing_lists"].Position; got != 0 {
			t.Errorf("mailing_lists position = %d, want 0", got)
		}
	})

	t.Run("an omitted requires_writer resolves to true", func(t *testing.T) {
		if !byKey["charter_agreed"].RequiresWriter {
			t.Error("charter_agreed requires_writer = false, want true")
		}
	})

	// The pointer exists precisely so this case survives expansion. Flattening
	// it to a plain bool would make an authored false indistinguishable from an
	// omitted field and silently promote it to true.
	t.Run("an authored requires_writer false is preserved", func(t *testing.T) {
		if byKey["membership_tiers"].RequiresWriter {
			t.Error("membership_tiers requires_writer = true, want the authored false")
		}
	})

	t.Run("the platform check travels onto the item", func(t *testing.T) {
		item := byKey["mailing_lists"]
		if item.StatusSource != model.SourcePlatform {
			t.Errorf("status_source = %q, want %q", item.StatusSource, model.SourcePlatform)
		}
		if item.PlatformCheck == nil || item.PlatformCheck.ResourceType != "mailing_list" {
			t.Errorf("platform_check = %+v, want mailing_list", item.PlatformCheck)
		}
	})

	t.Run("the action link placeholder is substituted once", func(t *testing.T) {
		want := "/project/project-1/charter"
		if got := byKey["charter_agreed"].ActionLink; got != want {
			t.Errorf("action_link = %q, want %q", got, want)
		}
	})

	t.Run("the due rule resolves against the announcement date", func(t *testing.T) {
		item := byKey["charter_agreed"]
		if item.DueDate == nil {
			t.Fatal("due_date is nil")
		}
		want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		if !item.DueDate.Equal(want) {
			t.Errorf("due_date = %v, want %v", item.DueDate, want)
		}
	})

	t.Run("every item starts not_started with a manual source by default", func(t *testing.T) {
		if got := byKey["charter_agreed"].Status; got != model.StatusNotStarted {
			t.Errorf("status = %q, want %q", got, model.StatusNotStarted)
		}
		if got := byKey["charter_agreed"].StatusSource; got != model.SourceManual {
			t.Errorf("status_source = %q, want %q", got, model.SourceManual)
		}
		if got := byKey["charter_agreed"].ChecklistType; got != model.ChecklistBoth {
			t.Errorf("checklist_type = %q, want %q", got, model.ChecklistBoth)
		}
	})

	// Sub-items are display detail with no bearing on the parent's status, so
	// they start pending regardless.
	t.Run("sub-items are copied in as pending", func(t *testing.T) {
		subItems := byKey["membership_tiers"].SubItems
		if len(subItems) != 1 {
			t.Fatalf("sub-items = %d, want 1", len(subItems))
		}
		if subItems[0].Key != "tier_draft" || subItems[0].Title != "Draft tiers" {
			t.Errorf("sub-item = %+v, want tier_draft/Draft tiers", subItems[0])
		}
		if subItems[0].Status != model.StatusNotStarted {
			t.Errorf("sub-item status = %q, want %q", subItems[0].Status, model.StatusNotStarted)
		}
	})

	// Nil would marshal to JSON null against a NOT NULL DEFAULT '[]' column.
	t.Run("an item with no sub-items gets an empty slice, not nil", func(t *testing.T) {
		if byKey["charter_agreed"].SubItems == nil {
			t.Error("sub_items is nil, want an empty slice")
		}
	})
}

// The reconcile loop runs on every replica with no reservation key, so this is
// the property that makes it safe: re-emitting the creation signal for every
// forming project at once must create zero duplicates and change zero statuses.
func TestExpandForIsIdempotentUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	f := newExpansionFixture(t, twoItemSections(), nil)

	// Seed first, then mutate a status, so a second expansion resetting it
	// would be visible rather than hidden behind everything being not_started.
	if _, err := f.expander.ExpandFor(ctx, "project-1"); err != nil {
		t.Fatalf("first ExpandFor() = %v, want no error", err)
	}
	formation, err := f.formations.GetByProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("GetByProject() = %v, want no error", err)
	}
	item, err := f.items.GetByKey(ctx, formation.UID, "charter_agreed")
	if err != nil {
		t.Fatalf("GetByKey() = %v, want no error", err)
	}
	done := model.StatusDone
	if _, err = f.items.Update(ctx, item.UID, item.Revision, port.ItemPatch{Status: &done}); err != nil {
		t.Fatalf("Update() = %v, want no error", err)
	}

	const replicas = 16
	var wg sync.WaitGroup
	createdCount := make([]bool, replicas)
	errs := make([]error, replicas)
	for i := range replicas {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			createdCount[i], errs[i] = f.expander.ExpandFor(ctx, "project-1")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d = %v, want no error — losing the race is success", i, err)
		}
	}
	for i, created := range createdCount {
		if created {
			t.Errorf("replica %d reported a creation, want none", i)
		}
	}

	items, err := f.items.ListByFormation(ctx, formation.UID)
	if err != nil {
		t.Fatalf("ListByFormation() = %v, want no error", err)
	}
	if len(items) != 3 {
		t.Errorf("items = %d, want 3 — expansion duplicated rows", len(items))
	}

	after, err := f.items.GetByKey(ctx, formation.UID, "charter_agreed")
	if err != nil {
		t.Fatalf("GetByKey() = %v, want no error", err)
	}
	if after.Status != model.StatusDone {
		t.Errorf("status = %q, want %q — expansion reset a status", after.Status, model.StatusDone)
	}
}

// A checklist with no due dates is usable; a project with no checklist is not.
// So every way the announcement read can fail has to leave due dates unset
// rather than stop creation.
func TestExpandForToleratesAMissingAnnouncementDate(t *testing.T) {
	tests := []struct {
		name     string
		projects port.ProjectReader
	}{
		{name: "no reader wired", projects: nil},
		{name: "the read fails", projects: stubProjects{err: context.DeadlineExceeded}},
		{name: "no date set", projects: stubProjects{announcement: ""}},
		{name: "the date is not a date", projects: stubProjects{announcement: "next spring"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newExpansionFixture(t, twoItemSections(), tc.projects)

			created, err := f.expander.ExpandFor(ctx, "project-1")
			if err != nil {
				t.Fatalf("ExpandFor() = %v, want no error", err)
			}
			if !created {
				t.Fatal("ExpandFor() created nothing")
			}

			formation, err := f.formations.GetByProject(ctx, "project-1")
			if err != nil {
				t.Fatalf("GetByProject() = %v, want no error", err)
			}
			item, err := f.items.GetByKey(ctx, formation.UID, "charter_agreed")
			if err != nil {
				t.Fatalf("GetByKey() = %v, want no error", err)
			}
			if item.DueDate != nil {
				t.Errorf("due_date = %v, want nil", item.DueDate)
			}
		})
	}
}

func TestExpandForWithNoPublishedTemplate(t *testing.T) {
	ctx := context.Background()
	formations := mock.NewFormationRepository()
	items := mock.NewItemRepository()
	templates := mock.NewTemplateRepository()
	uow := mock.NewUnitOfWork(formations, items, mock.NewActivityRepository(), templates)

	expander := NewExpander(NewTemplateSelector(templates), uow, nil)
	if _, err := expander.ExpandFor(ctx, "project-1"); err == nil {
		t.Error("ExpandFor() = nil, want an error when nothing is published")
	}
	// Nothing half-created: the template read happens before the write.
	if _, err := formations.GetByProject(ctx, "project-1"); err == nil {
		t.Error("a formation was created with no template to pin")
	}
}

func TestParseDueRule(t *testing.T) {
	tests := []struct {
		rule   string
		offset int
		ok     bool
	}{
		{rule: "announcement-30d", offset: 30, ok: true},
		{rule: "announcement-0d", offset: 0, ok: true},
		{rule: "announcement-30", ok: false},
		{rule: "announcement-d", ok: false},
		{rule: "announcement--5d", ok: false},
		{rule: "30d", ok: false},
		{rule: "", ok: false},
	}

	for _, tc := range tests {
		t.Run(tc.rule, func(t *testing.T) {
			offset, ok := ParseDueRule(tc.rule)
			if ok != tc.ok {
				t.Fatalf("ParseDueRule(%q) ok = %v, want %v", tc.rule, ok, tc.ok)
			}
			if ok && offset != tc.offset {
				t.Errorf("ParseDueRule(%q) = %d, want %d", tc.rule, offset, tc.offset)
			}
		})
	}
}
