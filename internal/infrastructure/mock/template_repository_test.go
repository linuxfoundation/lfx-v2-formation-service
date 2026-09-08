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
)

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
