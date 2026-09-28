// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// tickPast sleeps until time.Now() returns a value strictly after t.
func tickPast(t time.Time) {
	for !time.Now().After(t) {
		time.Sleep(time.Millisecond)
	}
}

// The double has to date publication the way the repository does. Every
// service-level test runs against this one, so a divergence here does not fail
// anything — it quietly makes those tests agree with behaviour Postgres does
// not have, which is the only way this particular difference could ever reach
// production.
func TestUpsertKeepsTheFirstPublicationTime(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepository()

	firstPublished := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	sections := []model.TemplateSection{{Key: "legal_and_entity", Title: "Legal and entity"}}

	if _, err := repo.Upsert(ctx, &model.Template{
		Name:        "published-at-test",
		Version:     1,
		State:       model.TemplatePublished,
		Priority:    100,
		Match:       "always",
		Sections:    sections,
		PublishedAt: &firstPublished,
	}); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	laterSeed := firstPublished.AddDate(0, 1, 0)
	again, err := repo.Upsert(ctx, &model.Template{
		Name:        "published-at-test",
		Version:     1,
		State:       model.TemplatePublished,
		Priority:    100,
		Match:       "always",
		Sections:    sections,
		PublishedAt: &laterSeed,
	})
	if err != nil {
		t.Fatalf("re-seed: %v, want no error", err)
	}
	if again.PublishedAt == nil || !again.PublishedAt.Equal(firstPublished) {
		t.Errorf("published_at = %v, want the original %v — a re-seed must not redate publication",
			again.PublishedAt, firstPublished)
	}
}

// The other half of the repository's COALESCE: nothing to preserve yet, so a
// draft promoted to published takes the incoming time.
func TestUpsertDatesAPromotedDraft(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepository()
	sections := []model.TemplateSection{{Key: "legal_and_entity", Title: "Legal and entity"}}

	if _, err := repo.Upsert(ctx, &model.Template{
		Name:     "promotion-test",
		Version:  1,
		State:    model.TemplateDraft,
		Priority: 100,
		Match:    "always",
		Sections: sections,
	}); err != nil {
		t.Fatalf("seeding the draft: %v", err)
	}

	promotedAt := time.Date(2026, 4, 2, 9, 30, 0, 0, time.UTC)
	promoted, err := repo.Upsert(ctx, &model.Template{
		Name:        "promotion-test",
		Version:     1,
		State:       model.TemplatePublished,
		Priority:    100,
		Match:       "always",
		Sections:    sections,
		PublishedAt: &promotedAt,
	})
	if err != nil {
		t.Fatalf("promoting the draft: %v, want no error", err)
	}
	if promoted.PublishedAt == nil || !promoted.PublishedAt.Equal(promotedAt) {
		t.Errorf("published_at = %v, want %v — a promoted draft must be dated",
			promoted.PublishedAt, promotedAt)
	}
}

// Mirrors the repository's guard: a version that has ever been published stays
// protected even once its state has moved on. Duplicated here for the same
// reason the PublishedAt double is — a divergence would let the seed command's
// guard pass in tests and fail against Postgres.
func TestUpsertProtectsContentAfterTheStateMovesOn(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepository()

	published := time.Now().UTC()
	original := []model.TemplateSection{{Key: "legal", Title: "Legal"}}
	if _, err := repo.Upsert(ctx, &model.Template{
		Name: "guard-test", Version: 1, State: model.TemplatePublished,
		Sections: original, PublishedAt: &published,
	}); err != nil {
		t.Fatalf("seeding the published version: %v", err)
	}
	if _, err := repo.Upsert(ctx, &model.Template{
		Name: "guard-test", Version: 1, State: model.TemplateDraft, Sections: original,
	}); err != nil {
		t.Fatalf("re-seeding identical content must still be allowed: %v", err)
	}

	_, err := repo.Upsert(ctx, &model.Template{
		Name: "guard-test", Version: 1, State: model.TemplateDraft,
		Sections: []model.TemplateSection{{Key: "legal", Title: "Rewritten"}},
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want domain.ErrConflict", err)
	}
}

// The same two-step bypass, from a publisher that supplies no publication time.
// The guard reads an undated row as never published, so without the model's
// default this sequence used to succeed at the third step: the seed command
// happens to set the timestamp, which meant the guarantee rested on the habit of
// one caller rather than on anything enforced.
func TestUpsertProtectsContentPublishedWithoutATimestamp(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepository()

	original := []model.TemplateSection{{Key: "legal", Title: "Legal"}}
	seeded, err := repo.Upsert(ctx, &model.Template{
		Name: "undated", Version: 1, State: model.TemplatePublished, Sections: original,
	})
	if err != nil {
		t.Fatalf("publishing without a timestamp: %v", err)
	}
	if seeded.PublishedAt == nil {
		t.Fatal("published_at = nil after publishing; the guard would read this version as never published")
	}

	if _, err := repo.Upsert(ctx, &model.Template{
		Name: "undated", Version: 1, State: model.TemplateDraft, Sections: original,
	}); err != nil {
		t.Fatalf("re-seeding identical content must still be allowed: %v", err)
	}

	_, err = repo.Upsert(ctx, &model.Template{
		Name: "undated", Version: 1, State: model.TemplateDraft,
		Sections: []model.TemplateSection{{Key: "legal", Title: "Rewritten"}},
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want domain.ErrConflict — the demotion must not release the content", err)
	}
}

// --- Admin API methods ---

func newDraft(t *testing.T, repo *TemplateRepository, name string) *model.Template {
	t.Helper()
	got, err := repo.Create(context.Background(), &model.Template{
		Name: name, Version: 1, Priority: 10, Match: "always",
		Sections: []model.TemplateSection{},
	})
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	return got
}

func TestCreateSetsTimestamps(t *testing.T) {
	before := time.Now()
	repo := NewTemplateRepository()
	got := newDraft(t, repo, "ts-test")
	if got.CreatedAt.IsZero() || got.CreatedAt.Before(before) {
		t.Errorf("CreatedAt = %v, want a recent non-zero time", got.CreatedAt)
	}
	if got.UpdatedAt.IsZero() || got.UpdatedAt.Before(before) {
		t.Errorf("UpdatedAt = %v, want a recent non-zero time", got.UpdatedAt)
	}
}

func TestCreateConflictOnDuplicateNameVersion(t *testing.T) {
	repo := NewTemplateRepository()
	newDraft(t, repo, "dup")
	_, err := repo.Create(context.Background(), &model.Template{Name: "dup", Version: 1})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate create = %v, want domain.ErrConflict", err)
	}
}

func TestUpdateAppliesPatchFields(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepository()
	draft := newDraft(t, repo, "upd")
	tickPast(draft.UpdatedAt)

	newPri := 42
	got, err := repo.Update(ctx, draft.UID, port.TemplatePatch{Priority: &newPri})
	if err != nil {
		t.Fatalf("Update() = %v", err)
	}
	if got.Priority != 42 {
		t.Errorf("priority = %d, want 42", got.Priority)
	}
	if !got.UpdatedAt.After(draft.UpdatedAt) {
		t.Errorf("UpdatedAt not advanced: before %v, after %v", draft.UpdatedAt, got.UpdatedAt)
	}
}

func TestUpdateRefusedWhenNotDraft(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepository()
	draft := newDraft(t, repo, "upd-pub")
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
	repo := NewTemplateRepository()
	draft := newDraft(t, repo, "empty-patch")

	_, err := repo.Update(ctx, draft.UID, port.TemplatePatch{})
	if !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("empty patch = %v, want domain.ErrInvalidRequest", err)
	}
}

func TestPublishSetsDates(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepository()
	draft := newDraft(t, repo, "pub")
	tickPast(draft.UpdatedAt)

	got, err := repo.Publish(ctx, draft.UID)
	if err != nil {
		t.Fatalf("Publish() = %v", err)
	}
	if got.State != model.TemplatePublished {
		t.Errorf("state = %q, want published", got.State)
	}
	if got.PublishedAt == nil {
		t.Error("published_at is nil after publish")
	}
	if !got.UpdatedAt.After(draft.UpdatedAt) {
		t.Errorf("UpdatedAt not advanced")
	}
}

func TestPublishRefusedWhenAlreadyPublished(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepository()
	draft := newDraft(t, repo, "pub2")
	if _, err := repo.Publish(ctx, draft.UID); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	_, err := repo.Publish(ctx, draft.UID)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("re-publish = %v, want domain.ErrConflict", err)
	}
}

func TestArchiveTransitionsFromDraftAndPublished(t *testing.T) {
	ctx := context.Background()
	repo := NewTemplateRepository()

	for _, name := range []string{"arc-draft", "arc-pub"} {
		draft := newDraft(t, repo, name)
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
	repo := NewTemplateRepository()
	draft := newDraft(t, repo, "arc2")
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
	repo := NewTemplateRepository()

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
