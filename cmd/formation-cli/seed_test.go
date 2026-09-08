// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

// The embedded content is data, so the tests that matter are the ones pinning
// the decisions it encodes. Each expectation below was a question someone had
// to answer, and a silent edit to any of them changes who can act on a row or
// which rows block going live.
func TestSeedContent(t *testing.T) {
	sections, err := loadSeedSections()
	if err != nil {
		t.Fatalf("loadSeedSections() = %v, want no error", err)
	}

	t.Run("the two sections are legal/entity then community/launch", func(t *testing.T) {
		want := []string{"legal_and_entity", "community_and_launch"}
		if len(sections) != len(want) {
			t.Fatalf("got %d sections, want %d", len(sections), len(want))
		}
		for i, key := range want {
			if sections[i].Key != key {
				t.Errorf("section %d = %q, want %q", i, sections[i].Key, key)
			}
		}
	})

	t.Run("17 items", func(t *testing.T) {
		if got := countItems(sections); got != 17 {
			t.Errorf("countItems() = %d, want 17", got)
		}
	})

	// Only legal/entity items may gate, and not all of them do. A gate added
	// under community/launch would make going live wait on work the section is
	// documented never to block on.
	t.Run("four gating items, all under legal and entity", func(t *testing.T) {
		var gating []string
		for _, section := range sections {
			for _, item := range section.Items {
				if !item.Gate {
					continue
				}
				gating = append(gating, item.Key)
				if section.Key != "legal_and_entity" {
					t.Errorf("item %q gates but sits under %q", item.Key, section.Key)
				}
			}
		}
		want := []string{
			"formation_review_packet",
			"charter_agreed",
			"contribution_agreement",
			"indepth_trademark_search_series_llc",
		}
		if len(gating) != len(want) {
			t.Fatalf("gating items = %v, want %v", gating, want)
		}
		for i, key := range want {
			if gating[i] != key {
				t.Errorf("gating[%d] = %q, want %q", i, gating[i], key)
			}
		}
	})

	// The platform/manual split is settled by rule, not by preference: a row is
	// platform only where a service can actually answer for it. These three are
	// the only ones with a countable resource in a service we can ask.
	t.Run("exactly three platform rows, each with a check", func(t *testing.T) {
		want := map[string]string{
			"repositories_github_owner": "repository",
			"mailing_lists":             "mailing_list",
			"tsc_kickoff":               "committee",
		}
		got := make(map[string]string)
		for _, section := range sections {
			for _, item := range section.Items {
				if item.StatusSource != model.SourcePlatform {
					continue
				}
				if item.PlatformCheck == nil {
					t.Fatalf("item %q is platform-sourced with no check", item.Key)
				}
				got[item.Key] = item.PlatformCheck.ResourceType
			}
		}
		if len(got) != len(want) {
			t.Fatalf("platform rows = %v, want %v", got, want)
		}
		for key, resource := range want {
			if got[key] != resource {
				t.Errorf("platform row %q checks %q, want %q", key, got[key], resource)
			}
		}
	})

	// requires_writer is omitted everywhere, which resolves to true. Encoding
	// it by omission is deliberate: an authored false would silently suppress
	// the prompt that elevates a viewer's access before an item is assigned.
	t.Run("every row requires a writer, by omission", func(t *testing.T) {
		for _, section := range sections {
			for _, item := range section.Items {
				if item.RequiresWriter != nil {
					t.Errorf("item %q sets requires_writer explicitly; the default carries it", item.Key)
				}
				if !item.RequiresWriterOrDefault() {
					t.Errorf("item %q resolves requires_writer to false", item.Key)
				}
			}
		}
	})

	// is_required is display metadata with a false default, and the template
	// marks the rows it genuinely requires rather than excusing the rest. The
	// content review has named none, so none are marked.
	t.Run("no row is marked required yet", func(t *testing.T) {
		for _, section := range sections {
			for _, item := range section.Items {
				if item.IsRequired {
					t.Errorf("item %q is marked required; the content review has named none", item.Key)
				}
			}
		}
	})

	t.Run("only chat_workspace has sub-items", func(t *testing.T) {
		for _, section := range sections {
			for _, item := range section.Items {
				if len(item.SubItems) > 0 && item.Key != "chat_workspace" {
					t.Errorf("item %q has %d sub-items, want none", item.Key, len(item.SubItems))
				}
			}
		}
		for _, section := range sections {
			for _, item := range section.Items {
				if item.Key == "chat_workspace" && len(item.SubItems) != 4 {
					t.Errorf("chat_workspace has %d sub-items, want 4", len(item.SubItems))
				}
			}
		}
	})
}

// Validation is the whole reason the content is loaded through a function
// rather than unmarshalled at the call site: these are the edits that cost a
// data migration if they reach the database unnoticed.
func TestValidateSectionsRefusals(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{
			name: "a hyphenated item key",
			json: `[{"key":"s","title":"S","items":[{"key":"charter-agreed","title":"T","status_source":"manual"}]}]`,
		},
		{
			name: "a capitalised item key",
			json: `[{"key":"s","title":"S","items":[{"key":"Charter","title":"T","status_source":"manual"}]}]`,
		},
		{
			name: "the same key in two sections",
			json: `[{"key":"a","title":"A","items":[{"key":"dup","title":"T","status_source":"manual"}]},
			        {"key":"b","title":"B","items":[{"key":"dup","title":"T","status_source":"manual"}]}]`,
		},
		{
			name: "an item with no title",
			json: `[{"key":"s","title":"S","items":[{"key":"k","status_source":"manual"}]}]`,
		},
		{
			name: "a section with no items",
			json: `[{"key":"s","title":"S","items":[]}]`,
		},
		{
			name: "a platform row with no check",
			json: `[{"key":"s","title":"S","items":[{"key":"k","title":"T","status_source":"platform"}]}]`,
		},
		{
			name: "a manual row carrying a check",
			json: `[{"key":"s","title":"S","items":[{"key":"k","title":"T","status_source":"manual",
			        "platform_check":{"resource_type":"repository","min_count":1}}]}]`,
		},
		{
			name: "a check nothing can fail",
			json: `[{"key":"s","title":"S","items":[{"key":"k","title":"T","status_source":"platform",
			        "platform_check":{"resource_type":"repository","min_count":0}}]}]`,
		},
		{
			name: "an unknown status_source",
			json: `[{"key":"s","title":"S","items":[{"key":"k","title":"T","status_source":"automatic"}]}]`,
		},
		{
			name: "a hyphenated sub-item key",
			json: `[{"key":"s","title":"S","items":[{"key":"k","title":"T","status_source":"manual",
			        "sub_items":[{"key":"sub-one","title":"Sub"}]}]}]`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sections []model.TemplateSection
			if err := json.Unmarshal([]byte(tc.json), &sections); err != nil {
				t.Fatalf("test fixture does not parse: %v", err)
			}
			if err := validateSections(sections); err == nil {
				t.Error("validateSections() = nil, want an error")
			}
		})
	}
}

// The seeded template has to be published and priced as the fallback, because
// the selection index is partial on state = 'published': a draft is invisible
// and the reconcile then finds no candidate for any project.
// These three fields are copied to the row verbatim, so a bad value has to be
// refused here or it reaches a reader — checklist_type as a 500 on every read of
// that checklist, due_rule as dates that silently never appear.
func TestValidateSectionsRefusesBadDisplayValues(t *testing.T) {
	base := func(item model.TemplateItem) []model.TemplateSection {
		return []model.TemplateSection{{
			Key:   "legal_and_entity",
			Title: "Legal and entity",
			Items: []model.TemplateItem{item},
		}}
	}
	ok := model.TemplateItem{
		Key: "charter_agreed", Title: "Charter agreed",
		OwnerTeam: "formation", StatusSource: model.SourceManual,
	}

	tests := []struct {
		name     string
		sections []model.TemplateSection
		wantErr  bool
	}{
		{
			name: "an unknown checklist_type is refused",
			sections: base(model.TemplateItem{
				Key: "charter_agreed", Title: "Charter agreed", OwnerTeam: "formation",
				StatusSource: model.SourceManual, ChecklistType: "externsl",
			}),
			wantErr: true,
		},
		{
			name: "an empty checklist_type is allowed, since insert defaults it",
			sections: base(model.TemplateItem{
				Key: "charter_agreed", Title: "Charter agreed", OwnerTeam: "formation",
				StatusSource: model.SourceManual, ChecklistType: "",
			}),
		},
		{
			name: "a due_rule missing its unit is refused",
			sections: base(model.TemplateItem{
				Key: "charter_agreed", Title: "Charter agreed", OwnerTeam: "formation",
				StatusSource: model.SourceManual, DueRule: "announcement-30",
			}),
			wantErr: true,
		},
		{
			name: "a well-formed due_rule is allowed",
			sections: base(model.TemplateItem{
				Key: "charter_agreed", Title: "Charter agreed", OwnerTeam: "formation",
				StatusSource: model.SourceManual, DueRule: "announcement-30d",
			}),
		},
		{
			name: "a section with no title is refused",
			sections: []model.TemplateSection{{
				Key: "legal_and_entity", Title: "", Items: []model.TemplateItem{ok},
			}},
			wantErr: true,
		},
		{
			name: "a repeated sub-item key within one item is refused",
			sections: base(model.TemplateItem{
				Key: "charter_agreed", Title: "Charter agreed", OwnerTeam: "formation",
				StatusSource: model.SourceManual,
				SubItems: []model.TemplateSubItem{
					{Key: "draft", Title: "Draft"},
					{Key: "draft", Title: "Draft again"},
				},
			}),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSections(tc.sections)
			if tc.wantErr && err == nil {
				t.Error("validateSections() = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateSections() = %v, want no error", err)
			}
		})
	}
}

func TestBuildSeedTemplatePublishes(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	tpl := buildSeedTemplate([]model.TemplateSection{{Key: "s", Title: "S"}}, now)

	if tpl.State != model.TemplatePublished {
		t.Errorf("state = %q, want %q", tpl.State, model.TemplatePublished)
	}
	if tpl.PublishedAt == nil {
		t.Fatal("published_at is nil on a published template")
	}
	if !tpl.PublishedAt.Equal(now) {
		t.Errorf("published_at = %v, want %v", tpl.PublishedAt, now)
	}
	if tpl.Match != seedTemplateMatch {
		t.Errorf("match = %q, want %q", tpl.Match, seedTemplateMatch)
	}
	if tpl.Version != seedTemplateVersion {
		t.Errorf("version = %d, want %d", tpl.Version, seedTemplateVersion)
	}
}

// Re-running the job must be a no-op rather than a second template: the
// reconcile picks by priority and first match, so two published rows with the
// same priority would make selection depend on row order.
// A streaming decoder stops at the end of the first value, so anything after it
// is invisible. That is the shape a truncated edit or a bad merge leaves behind,
// and the seed reporting success on a template it only half read is worse than
// failing.
func TestDecodeSectionsRefusesTrailingContent(t *testing.T) {
	one := `[{"key":"sec","title":"Section","items":[]}]`

	for name, raw := range map[string]string{
		"a second document": one + one,
		"trailing garbage":  one + " nonsense",
		"trailing object":   one + ` {"key":"extra"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeSections([]byte(raw)); err == nil {
				t.Error("decodeSections() = no error, want a refusal of the content after the first document")
			}
		})
	}

	if _, err := decodeSections([]byte(one + "\n")); err != nil {
		t.Errorf("decodeSections() with trailing whitespace = %v, want no error", err)
	}
}

func TestRunSeedIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := mock.NewTemplateRepository()

	for i := range 3 {
		if err := runSeed(ctx, repo); err != nil {
			t.Fatalf("runSeed() run %d = %v, want no error", i+1, err)
		}
	}

	published, err := repo.ListPublished(ctx)
	if err != nil {
		t.Fatalf("ListPublished() = %v, want no error", err)
	}
	if len(published) != 1 {
		t.Fatalf("published templates = %d, want 1", len(published))
	}
	if got := countItems(published[0].Sections); got != 17 {
		t.Errorf("seeded items = %d, want 17", got)
	}
}

// Editing content without bumping the version has to fail rather than quietly
// rewrite a published row. Checklists pin the version they expanded from, so an
// in-place edit would change what every existing pin refers to, and two
// checklists could claim the same version having been built from different
// content.
func TestSeedRefusesEditingAPublishedVersion(t *testing.T) {
	ctx := context.Background()
	repo := mock.NewTemplateRepository()

	if err := runSeed(ctx, repo); err != nil {
		t.Fatalf("runSeed() = %v, want no error", err)
	}

	edited := buildSeedTemplate([]model.TemplateSection{{
		Key:   "changed",
		Title: "Changed after publication",
	}}, time.Now())

	_, err := repo.Upsert(ctx, edited)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("Upsert() with edited content = %v, want domain.ErrConflict", err)
	}

	published, err := repo.ListPublished(ctx)
	if err != nil {
		t.Fatalf("ListPublished() = %v, want no error", err)
	}
	if got := countItems(published[0].Sections); got != 17 {
		t.Errorf("items after the refused edit = %d, want the published 17 untouched", got)
	}
}
