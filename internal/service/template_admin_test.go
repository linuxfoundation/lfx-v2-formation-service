// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
)

func templateService(t *testing.T) (*Service, *mock.TemplateRepository) {
	t.Helper()
	templates := mock.NewTemplateRepository()
	s := NewService(WithTemplates(templates))
	return s, templates
}

// --- CreateTemplate ---

func TestCreateTemplateReturnsNewDraft(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	got, err := s.CreateTemplate(ctx, &svc.CreateTemplatePayload{
		Version:         "1",
		Name:            "base",
		TemplateVersion: 1,
		Priority:        10,
		Match:           "always",
		Sections:        []any{},
	})
	if err != nil {
		t.Fatalf("CreateTemplate() = %v, want no error", err)
	}
	if got.UID == "" {
		t.Error("UID is empty")
	}
	if got.State != "draft" {
		t.Errorf("state = %q, want draft", got.State)
	}
	if got.TemplateVersion != 1 {
		t.Errorf("template_version = %d, want 1", got.TemplateVersion)
	}
}

func TestCreateTemplateDuplicateNameVersion(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	p := &svc.CreateTemplatePayload{
		Version: "1", Name: "dup", TemplateVersion: 1, Priority: 10, Match: "always", Sections: []any{},
	}
	if _, err := s.CreateTemplate(ctx, p); err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err := s.CreateTemplate(ctx, p)
	var te *svc.TemplateError
	if !errors.As(err, &te) || te.Reason != "template_already_exists" {
		t.Fatalf("duplicate create = %v, want TemplateError{reason:template_already_exists}", err)
	}
}

func TestCreateTemplateInvalidSectionsJSON(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	// sections that cannot be decoded as []model.TemplateSection
	_, err := s.CreateTemplate(ctx, &svc.CreateTemplatePayload{
		Version: "1", Name: "bad", TemplateVersion: 1, Priority: 10, Match: "always",
		Sections: map[string]any{"not": "an array"},
	})
	var te *svc.TemplateError
	if !errors.As(err, &te) || te.Reason != "invalid_sections" {
		t.Fatalf("bad sections = %v, want TemplateError{reason:invalid_sections}", err)
	}
}

// --- GetTemplate ---

func TestGetTemplateNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	_, err := s.GetTemplate(ctx, &svc.GetTemplatePayload{
		Version: "1",
		UID:     "00000000-0000-0000-0000-000000000001",
	})
	var te *svc.TemplateError
	if !errors.As(err, &te) || te.Reason != "not_found" {
		t.Fatalf("missing uid = %v, want TemplateError{reason:not_found}", err)
	}
}

func TestGetTemplateMalformedUID(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	_, err := s.GetTemplate(ctx, &svc.GetTemplatePayload{Version: "1", UID: "not-a-uuid"})
	var te *svc.TemplateError
	if !errors.As(err, &te) || te.Reason != "not_found" {
		t.Fatalf("malformed uid = %v, want TemplateError{reason:not_found}", err)
	}
}

// --- UpdateTemplate ---

func TestUpdateTemplateAppliesFields(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	created, err := s.CreateTemplate(ctx, &svc.CreateTemplatePayload{
		Version: "1", Name: "upd", TemplateVersion: 1, Priority: 5, Match: "always", Sections: []any{},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	newPri := 99
	updated, err := s.UpdateTemplate(ctx, &svc.UpdateTemplatePayload{
		Version:  "1",
		UID:      created.UID,
		Priority: &newPri,
	})
	if err != nil {
		t.Fatalf("UpdateTemplate() = %v", err)
	}
	if updated.Priority != 99 {
		t.Errorf("priority = %d, want 99", updated.Priority)
	}
}

func TestUpdateTemplateNoFieldsReturnsError(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	created, _ := s.CreateTemplate(ctx, &svc.CreateTemplatePayload{
		Version: "1", Name: "noop", TemplateVersion: 1, Priority: 5, Match: "always", Sections: []any{},
	})

	_, err := s.UpdateTemplate(ctx, &svc.UpdateTemplatePayload{Version: "1", UID: created.UID})
	var te *svc.TemplateError
	if !errors.As(err, &te) || te.Reason != "no_fields_to_update" {
		t.Fatalf("empty patch = %v, want TemplateError{reason:no_fields_to_update}", err)
	}
}

func TestUpdateTemplateRefusedOnPublished(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	created, _ := s.CreateTemplate(ctx, &svc.CreateTemplatePayload{
		Version: "1", Name: "pub-upd", TemplateVersion: 1, Priority: 5, Match: "always", Sections: []any{},
	})
	if _, err := s.PublishTemplate(ctx, &svc.PublishTemplatePayload{Version: "1", UID: created.UID}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	newPri := 1
	_, err := s.UpdateTemplate(ctx, &svc.UpdateTemplatePayload{
		Version: "1", UID: created.UID, Priority: &newPri,
	})
	var te *svc.TemplateError
	if !errors.As(err, &te) || te.Reason != "template_not_draft" {
		t.Fatalf("update published = %v, want TemplateError{reason:template_not_draft}", err)
	}
}

// --- PublishTemplate ---

func TestPublishTemplateSetsPublishedAt(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	created, _ := s.CreateTemplate(ctx, &svc.CreateTemplatePayload{
		Version: "1", Name: "pub", TemplateVersion: 1, Priority: 5, Match: "always", Sections: []any{},
	})
	published, err := s.PublishTemplate(ctx, &svc.PublishTemplatePayload{Version: "1", UID: created.UID})
	if err != nil {
		t.Fatalf("PublishTemplate() = %v", err)
	}
	if published.State != "published" {
		t.Errorf("state = %q, want published", published.State)
	}
	if published.PublishedAt == nil {
		t.Error("published_at is nil after publish")
	}
}

func TestPublishTemplateRefusedWhenAlreadyPublished(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	created, _ := s.CreateTemplate(ctx, &svc.CreateTemplatePayload{
		Version: "1", Name: "pub2", TemplateVersion: 1, Priority: 5, Match: "always", Sections: []any{},
	})
	if _, err := s.PublishTemplate(ctx, &svc.PublishTemplatePayload{Version: "1", UID: created.UID}); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	_, err := s.PublishTemplate(ctx, &svc.PublishTemplatePayload{Version: "1", UID: created.UID})
	var te *svc.TemplateError
	if !errors.As(err, &te) || te.Reason != "template_not_draft" {
		t.Fatalf("re-publish = %v, want TemplateError{reason:template_not_draft}", err)
	}
}

// --- ArchiveTemplate ---

func TestArchiveTemplateTransitions(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	for _, name := range []string{"arc-draft", "arc-published"} {
		created, _ := s.CreateTemplate(ctx, &svc.CreateTemplatePayload{
			Version: "1", Name: name, TemplateVersion: 1, Priority: 5, Match: "always", Sections: []any{},
		})
		if name == "arc-published" {
			if _, err := s.PublishTemplate(ctx, &svc.PublishTemplatePayload{Version: "1", UID: created.UID}); err != nil {
				t.Fatalf("publish %s: %v", name, err)
			}
		}
		archived, err := s.ArchiveTemplate(ctx, &svc.ArchiveTemplatePayload{Version: "1", UID: created.UID})
		if err != nil {
			t.Fatalf("Archive(%s) = %v", name, err)
		}
		if archived.State != "archived" {
			t.Errorf("%s: state = %q, want archived", name, archived.State)
		}
	}
}

func TestArchiveTemplateRefusedWhenAlreadyArchived(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	created, _ := s.CreateTemplate(ctx, &svc.CreateTemplatePayload{
		Version: "1", Name: "arc2", TemplateVersion: 1, Priority: 5, Match: "always", Sections: []any{},
	})
	if _, err := s.ArchiveTemplate(ctx, &svc.ArchiveTemplatePayload{Version: "1", UID: created.UID}); err != nil {
		t.Fatalf("first archive: %v", err)
	}

	_, err := s.ArchiveTemplate(ctx, &svc.ArchiveTemplatePayload{Version: "1", UID: created.UID})
	var te *svc.TemplateError
	if !errors.As(err, &te) || te.Reason != "template_already_archived" {
		t.Fatalf("re-archive = %v, want TemplateError{reason:template_already_archived}", err)
	}
}

// --- templateToWire ---

func TestTemplateToWireOmitsEmptyAuthorAndNilPublishedAt(t *testing.T) {
	ctx := context.Background()
	s, _ := templateService(t)

	created, err := s.CreateTemplate(ctx, &svc.CreateTemplatePayload{
		Version: "1", Name: "wire", TemplateVersion: 1, Priority: 5, Match: "always", Sections: []any{},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Author != nil {
		t.Errorf("author = %v, want nil for empty string", created.Author)
	}
	if created.PublishedAt != nil {
		t.Errorf("published_at = %v, want nil for unpublished draft", created.PublishedAt)
	}
}

// --- withTemplateReason ---

func TestWithTemplateReasonPassesThroughTypedReasonErrors(t *testing.T) {
	re := domain.NewReasonError(domain.ErrConflict, "template_already_archived")
	got := withTemplateReason(re)
	var out *domain.ReasonError
	if !errors.As(got, &out) || out.Reason != "template_already_archived" {
		t.Errorf("got reason %q, want template_already_archived", out.Reason)
	}
}

func TestWithTemplateReasonWrapsBareSentinels(t *testing.T) {
	cases := []struct {
		err    error
		reason string
	}{
		{domain.ErrNotFound, "not_found"},
		{domain.ErrConflict, "template_not_draft"},
		{domain.ErrInvalidRequest, "no_fields_to_update"},
	}
	for _, c := range cases {
		got := withTemplateReason(c.err)
		var re *domain.ReasonError
		if !errors.As(got, &re) || re.Reason != c.reason {
			t.Errorf("withTemplateReason(%v) reason = %q, want %q", c.err, re.Reason, c.reason)
		}
	}
}
