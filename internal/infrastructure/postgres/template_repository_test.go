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
