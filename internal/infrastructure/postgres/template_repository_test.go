// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

func oneSection(title string) []model.TemplateSection {
	return []model.TemplateSection{{
		Key:   "legal_and_entity",
		Title: title,
		Items: []model.TemplateItem{{
			Key:          "charter_agreed",
			Title:        "Charter agreed",
			StatusSource: model.SourceManual,
		}},
	}}
}

// A published version's content is immutable, and it has to be the database that
// says so: checklists pin the version they expanded from, so an in-place edit
// changes what every existing pin refers to and two checklists can end up
// claiming the same version having been expanded from different content.
//
// Re-seeding the identical thing stays a no-op, because that is what makes
// re-running the seed job safe.
func TestUpsertRefusesEditingAPublishedVersion(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))

	published, err := repo.Upsert(ctx, &model.Template{
		Name:     "immutability-test",
		Version:  1,
		State:    model.TemplatePublished,
		Priority: 100,
		Match:    "always",
		Sections: oneSection("Legal and entity"),
	})
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}

	t.Run("an identical re-seed is a no-op", func(t *testing.T) {
		again, err := repo.Upsert(ctx, &model.Template{
			Name:     "immutability-test",
			Version:  1,
			State:    model.TemplatePublished,
			Priority: 100,
			Match:    "always",
			Sections: oneSection("Legal and entity"),
		})
		if err != nil {
			t.Fatalf("identical re-seed: %v, want no error", err)
		}
		if again.UID != published.UID {
			t.Errorf("uid = %s, want the original %s", again.UID, published.UID)
		}
	})

	t.Run("an edit is refused", func(t *testing.T) {
		_, err := repo.Upsert(ctx, &model.Template{
			Name:     "immutability-test",
			Version:  1,
			State:    model.TemplatePublished,
			Priority: 100,
			Match:    "always",
			Sections: oneSection("Retitled after publication"),
		})
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("edited re-seed = %v, want domain.ErrConflict", err)
		}

		stored, err := repo.Get(ctx, published.UID)
		if err != nil {
			t.Fatalf("Get() = %v", err)
		}
		if stored.Sections[0].Title != "Legal and entity" {
			t.Errorf("stored title = %q, want the published content untouched", stored.Sections[0].Title)
		}
	})

	t.Run("a new version is accepted alongside it", func(t *testing.T) {
		if _, err := repo.Upsert(ctx, &model.Template{
			Name:     "immutability-test",
			Version:  2,
			State:    model.TemplatePublished,
			Priority: 100,
			Match:    "always",
			Sections: oneSection("Retitled after publication"),
		}); err != nil {
			t.Fatalf("seeding v2: %v, want no error", err)
		}
	})
}

// A draft is still open to change: the rule protects what has been published,
// not a version nobody can have expanded from.
func TestUpsertStillReplacesADraft(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))

	if _, err := repo.Upsert(ctx, &model.Template{
		Name:     "draft-test",
		Version:  1,
		State:    model.TemplateDraft,
		Priority: 100,
		Match:    "always",
		Sections: oneSection("First attempt"),
	}); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	edited, err := repo.Upsert(ctx, &model.Template{
		Name:     "draft-test",
		Version:  1,
		State:    model.TemplateDraft,
		Priority: 100,
		Match:    "always",
		Sections: oneSection("Second attempt"),
	})
	if err != nil {
		t.Fatalf("editing a draft: %v, want no error", err)
	}
	if edited.Sections[0].Title != "Second attempt" {
		t.Errorf("title = %q, want the edit applied", edited.Sections[0].Title)
	}
}

// published_at dates the first publication, not the most recent seed, and the
// COALESCE in the upsert is the only thing implementing that. Both directions
// are checked here because the clause has two halves and each fails silently on
// its own: without the existing value it moves forward on every re-seed, and
// without the incoming one a draft promoted to published keeps a NULL beside
// state = 'published'.
func TestUpsertKeepsTheFirstPublicationTime(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))

	firstPublished := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	seeded, err := repo.Upsert(ctx, &model.Template{
		Name:        "published-at-test",
		Version:     1,
		State:       model.TemplatePublished,
		Priority:    100,
		Match:       "always",
		Sections:    oneSection("Legal and entity"),
		PublishedAt: &firstPublished,
	})
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if seeded.PublishedAt == nil || !seeded.PublishedAt.Equal(firstPublished) {
		t.Fatalf("published_at = %v, want %v on the first seed", seeded.PublishedAt, firstPublished)
	}

	t.Run("a later identical re-seed does not move it", func(t *testing.T) {
		laterSeed := firstPublished.AddDate(0, 1, 0)
		again, err := repo.Upsert(ctx, &model.Template{
			Name:        "published-at-test",
			Version:     1,
			State:       model.TemplatePublished,
			Priority:    100,
			Match:       "always",
			Sections:    oneSection("Legal and entity"),
			PublishedAt: &laterSeed,
		})
		if err != nil {
			t.Fatalf("re-seed: %v, want no error", err)
		}
		if again.PublishedAt == nil || !again.PublishedAt.Equal(firstPublished) {
			t.Errorf("published_at = %v, want the original %v — a re-seed must not redate publication",
				again.PublishedAt, firstPublished)
		}
	})

	// The other half: nothing to preserve yet, so the incoming value lands.
	t.Run("promoting a draft populates it", func(t *testing.T) {
		if _, err := repo.Upsert(ctx, &model.Template{
			Name:     "published-at-draft",
			Version:  1,
			State:    model.TemplateDraft,
			Priority: 100,
			Match:    "always",
			Sections: oneSection("Legal and entity"),
		}); err != nil {
			t.Fatalf("seeding the draft: %v", err)
		}

		promotedAt := time.Date(2026, 4, 2, 9, 30, 0, 0, time.UTC)
		promoted, err := repo.Upsert(ctx, &model.Template{
			Name:        "published-at-draft",
			Version:     1,
			State:       model.TemplatePublished,
			Priority:    100,
			Match:       "always",
			Sections:    oneSection("Legal and entity"),
			PublishedAt: &promotedAt,
		})
		if err != nil {
			t.Fatalf("promoting the draft: %v, want no error", err)
		}
		if promoted.PublishedAt == nil || !promoted.PublishedAt.Equal(promotedAt) {
			t.Errorf("published_at = %v, want %v — a promoted draft must be dated",
				promoted.PublishedAt, promotedAt)
		}
	})
}

// Selection walks this list and takes the first match, so ordering is the whole
// contract. Publishing a version does not retire the one before it, which leaves
// both here at one priority — and priority alone would let the database return
// them in either order.
func TestListPublishedOrdersNewestVersionFirstWithinAPriority(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))

	for _, version := range []int{1, 3, 2} {
		if _, err := repo.Upsert(ctx, &model.Template{
			Name:     "ordering-test",
			Version:  version,
			State:    model.TemplatePublished,
			Priority: 100,
			Match:    "always",
			Sections: oneSection("Legal and entity"),
		}); err != nil {
			t.Fatalf("seeding v%d: %v", version, err)
		}
	}
	// Lower priority sorts ahead of every version of the above.
	if _, err := repo.Upsert(ctx, &model.Template{
		Name:     "ordering-test-preferred",
		Version:  1,
		State:    model.TemplatePublished,
		Priority: 10,
		Match:    "always",
		Sections: oneSection("Legal and entity"),
	}); err != nil {
		t.Fatalf("seeding the preferred template: %v", err)
	}

	got, err := repo.ListPublished(ctx)
	if err != nil {
		t.Fatalf("ListPublished() = %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("published = %d, want 4", len(got))
	}
	if got[0].Name != "ordering-test-preferred" {
		t.Errorf("first = %q, want the lower priority to win outright", got[0].Name)
	}
	for i, wantVersion := range []int{3, 2, 1} {
		if got[i+1].Version != wantVersion {
			t.Errorf("candidate %d = v%d, want v%d — newest version first within one priority",
				i+1, got[i+1].Version, wantVersion)
		}
	}
}

// The guard covers a version that has ever been published, not one that is
// published at this moment. Gating on the current state alone left a two-step
// way around it: re-seed the same content under a different state, which passes
// because only content is compared, and the row is then no longer published, so
// the next seed may rewrite content some checklist already expanded from.
//
// Nothing reachable demotes a template today — the seed command always publishes
// and no code sets the archived state — so this is the invariant being made
// independent of that rather than a live path being closed.
func TestPublishedContentStaysProtectedAfterTheStateMovesOn(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))

	published := time.Now().UTC()
	original := []model.TemplateSection{{
		Key:   "legal",
		Title: "Legal",
		Items: []model.TemplateItem{{Key: "entity", Title: "Entity formed", StatusSource: model.SourceManual}},
	}}
	stored, err := repo.Upsert(ctx, &model.Template{
		Name: "guard-test", Version: 1, State: model.TemplatePublished,
		Priority: 100, Match: "always", Sections: original, PublishedAt: &published,
	})
	if err != nil {
		t.Fatalf("seeding the published version: %v", err)
	}

	// Step one: identical content, so the content check passes, but the state
	// moves off published.
	if _, err := repo.Upsert(ctx, &model.Template{
		Name: "guard-test", Version: 1, State: model.TemplateDraft,
		Priority: 100, Match: "always", Sections: original,
	}); err != nil {
		t.Fatalf("re-seeding identical content must still be allowed: %v", err)
	}

	// Step two: the rewrite the guard exists to refuse.
	edited := []model.TemplateSection{{
		Key:   "legal",
		Title: "Legal",
		Items: []model.TemplateItem{{Key: "entity", Title: "Something else", StatusSource: model.SourceManual}},
	}}
	if _, err := repo.Upsert(ctx, &model.Template{
		Name: "guard-test", Version: 1, State: model.TemplateDraft,
		Priority: 100, Match: "always", Sections: edited,
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want domain.ErrConflict — a version that has been published stays protected", err)
	}

	after, err := repo.Get(ctx, stored.UID)
	if err != nil {
		t.Fatalf("reading the stored template: %v", err)
	}
	if got := after.Sections[0].Items[0].Title; got != "Entity formed" {
		t.Errorf("stored title = %q, want the published content untouched", got)
	}
}

// --- Admin API methods (Create, Update, Publish, Archive, List) ---

func adminDraft(t *testing.T, repo *TemplateRepo, name string, version int) *model.Template {
	t.Helper()
	got, err := repo.Create(context.Background(), &model.Template{
		Name: name, Version: version, Priority: 10, Match: "always",
		Sections: oneSection("Legal and entity"),
	})
	if err != nil {
		t.Fatalf("Create(%s v%d): %v", name, version, err)
	}
	return got
}

// Update must actually persist the patched fields. This is the database-side
// assertion for the Bun Column+Set interaction that was invisible without a
// round-trip check.
func TestUpdatePersistsAllPatchFields(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))
	draft := adminDraft(t, repo, "persist-test", 1)

	newPri := 77
	newMatch := "always"
	newAuthor := "alice"
	newSections := []model.TemplateSection{{
		Key:   "updated",
		Title: "Updated section",
		Items: []model.TemplateItem{{Key: "item1", Title: "Item one", StatusSource: model.SourceManual}},
	}}
	updated, err := repo.Update(ctx, draft.UID, port.TemplatePatch{
		Priority: &newPri,
		Match:    &newMatch,
		Author:   &newAuthor,
		Sections: &newSections,
	})
	if err != nil {
		t.Fatalf("Update() = %v", err)
	}

	if updated.Priority != 77 {
		t.Errorf("priority = %d, want 77", updated.Priority)
	}
	if updated.Author != "alice" {
		t.Errorf("author = %q, want alice", updated.Author)
	}
	if len(updated.Sections) == 0 || updated.Sections[0].Key != "updated" {
		t.Errorf("sections not persisted; got %+v", updated.Sections)
	}

	// Re-fetch to confirm the database reflects the change.
	reloaded, err := repo.Get(ctx, draft.UID)
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if reloaded.Priority != 77 {
		t.Errorf("reloaded priority = %d, want 77 — Update did not actually write to the database", reloaded.Priority)
	}
	if reloaded.Author != "alice" {
		t.Errorf("reloaded author = %q, want alice", reloaded.Author)
	}
}

func TestUpdateRefusedWhenNotDraft(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))
	draft := adminDraft(t, repo, "upd-pub", 1)
	if _, err := repo.Publish(ctx, draft.UID); err != nil {
		t.Fatalf("publish: %v", err)
	}

	newPri := 1
	_, err := repo.Update(ctx, draft.UID, port.TemplatePatch{Priority: &newPri})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("update published = %v, want domain.ErrConflict", err)
	}
}

func TestUpdateEmptyPatchReturnsInvalidRequest(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))
	draft := adminDraft(t, repo, "empty-patch", 1)

	_, err := repo.Update(ctx, draft.UID, port.TemplatePatch{})
	if !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("empty patch = %v, want domain.ErrInvalidRequest", err)
	}
}

func TestCreateConflictOnDuplicateNameVersion(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))
	adminDraft(t, repo, "dup", 1)

	_, err := repo.Create(ctx, &model.Template{Name: "dup", Version: 1, Match: "always"})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate create = %v, want domain.ErrConflict", err)
	}
}

func TestPublishSetsPublishedAtAndLeavesArchiveAlone(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))
	draft := adminDraft(t, repo, "pub-test", 1)

	published, err := repo.Publish(ctx, draft.UID)
	if err != nil {
		t.Fatalf("Publish() = %v", err)
	}
	if published.State != model.TemplatePublished {
		t.Errorf("state = %q, want published", published.State)
	}
	if published.PublishedAt == nil {
		t.Error("published_at is nil after Publish")
	}

	// Archive must not touch published_at.
	archived, err := repo.Archive(ctx, draft.UID)
	if err != nil {
		t.Fatalf("Archive after Publish: %v", err)
	}
	if archived.PublishedAt == nil || !archived.PublishedAt.Equal(*published.PublishedAt) {
		t.Errorf("published_at changed by Archive: %v → %v", published.PublishedAt, archived.PublishedAt)
	}
}

func TestPublishRefusedWhenAlreadyPublished(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))
	draft := adminDraft(t, repo, "pub2", 1)
	if _, err := repo.Publish(ctx, draft.UID); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	_, err := repo.Publish(ctx, draft.UID)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("re-publish = %v, want domain.ErrConflict", err)
	}
}

func TestArchiveAllowedFromDraftAndPublished(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))

	for _, name := range []string{"arc-draft", "arc-pub"} {
		draft := adminDraft(t, repo, name, 1)
		if name == "arc-pub" {
			if _, err := repo.Publish(ctx, draft.UID); err != nil {
				t.Fatalf("publish %s: %v", name, err)
			}
		}
		got, err := repo.Archive(ctx, draft.UID)
		if err != nil {
			t.Fatalf("Archive(%s) = %v", name, err)
		}
		if got.State != model.TemplateArchived {
			t.Errorf("%s: state = %q, want archived", name, got.State)
		}
	}
}

func TestArchiveRefusedWhenAlreadyArchived(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))
	draft := adminDraft(t, repo, "arc2", 1)
	if _, err := repo.Archive(ctx, draft.UID); err != nil {
		t.Fatalf("first archive: %v", err)
	}
	_, err := repo.Archive(ctx, draft.UID)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("re-archive = %v, want domain.ErrConflict", err)
	}
}

func TestListOrdersByNameThenVersion(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepo(testDB(t))

	for _, pair := range [][2]any{{"beta", 2}, {"alpha", 1}, {"alpha", 2}, {"beta", 1}} {
		if _, err := repo.Create(ctx, &model.Template{
			Name: pair[0].(string), Version: pair[1].(int), Match: "always",
		}); err != nil {
			t.Fatalf("create %v v%v: %v", pair[0], pair[1], err)
		}
	}

	got, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	want := [][2]any{{"alpha", 1}, {"alpha", 2}, {"beta", 1}, {"beta", 2}}
	for i, w := range want {
		if got[i].Name != w[0].(string) || got[i].Version != w[1].(int) {
			t.Errorf("List()[%d] = {%s v%d}, want {%s v%d}",
				i, got[i].Name, got[i].Version, w[0], w[1])
		}
	}
}
