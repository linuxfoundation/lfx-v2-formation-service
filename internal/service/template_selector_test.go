// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

func publish(t *testing.T, repo *mock.TemplateRepository, name string, priority int, match string) {
	t.Helper()
	_, err := repo.Upsert(context.Background(), &model.Template{
		Name:     name,
		Version:  1,
		State:    model.TemplatePublished,
		Priority: priority,
		Match:    match,
	})
	if err != nil {
		t.Fatalf("Upsert(%q) = %v, want no error", name, err)
	}
}

func TestSelectTakesTheLowestPriorityMatch(t *testing.T) {
	ctx := context.Background()
	repo := mock.NewTemplateRepository()
	publish(t, repo, "fallback", 100, MatchAlways)
	publish(t, repo, "specific", 10, MatchAlways)

	got, err := NewTemplateSelector(repo).Select(ctx)
	if err != nil {
		t.Fatalf("Select() = %v, want no error", err)
	}
	if got.Name != "specific" {
		t.Errorf("Select() = %q, want %q — lower priority wins", got.Name, "specific")
	}
}

// A rule this build cannot read must be skipped, never treated as firing.
// Selection walks in priority order, so a template that sorts first and carries
// an unknown rule would otherwise capture every project.
func TestSelectSkipsUnrecognisedRules(t *testing.T) {
	ctx := context.Background()
	repo := mock.NewTemplateRepository()
	publish(t, repo, "future", 10, "project_is_umbrella")
	publish(t, repo, "fallback", 100, MatchAlways)

	got, err := NewTemplateSelector(repo).Select(ctx)
	if err != nil {
		t.Fatalf("Select() = %v, want no error", err)
	}
	if got.Name != "fallback" {
		t.Errorf("Select() = %q, want %q", got.Name, "fallback")
	}
}

// A draft is not a candidate. The repository filters it, and this pins that the
// selector depends on that filter rather than re-checking state itself — if the
// filter ever moves, this fails rather than silently expanding selection.
func TestSelectIgnoresDrafts(t *testing.T) {
	ctx := context.Background()
	repo := mock.NewTemplateRepository()
	_, err := repo.Upsert(ctx, &model.Template{
		Name: "draft", Version: 1, State: model.TemplateDraft, Priority: 1, Match: MatchAlways,
	})
	if err != nil {
		t.Fatalf("Upsert() = %v, want no error", err)
	}

	if _, err := NewTemplateSelector(repo).Select(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Select() = %v, want %v", err, domain.ErrNotFound)
	}
}

func TestSelectWithNoTemplates(t *testing.T) {
	ctx := context.Background()

	if _, err := NewTemplateSelector(mock.NewTemplateRepository()).Select(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Select() = %v, want %v", err, domain.ErrNotFound)
	}
}

// Every rule the seeded content can carry has to be one the selector fires on,
// or the seed publishes a template nothing will ever pick.
func TestMatchAlwaysFires(t *testing.T) {
	if !matches(MatchAlways) {
		t.Errorf("matches(%q) = false, want true", MatchAlways)
	}
	if matches("") {
		t.Error(`matches("") = true, want false`)
	}
}
