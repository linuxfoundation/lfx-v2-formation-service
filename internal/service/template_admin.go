// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/log"
)

const (
	templateReasonNotFound         = "not_found"
	templateReasonNotDraft         = "template_not_draft"
	templateReasonAlreadyArchived  = "template_already_archived"
	templateReasonAlreadyExists    = "template_already_exists"
	templateReasonNoFieldsToUpdate = "no_fields_to_update"
	templateReasonInvalidSections  = "invalid_sections"
)

var templateReasonMessages = map[string]string{
	templateReasonNotFound:         "no template with that UID",
	templateReasonNotDraft:         "only draft templates may be modified",
	templateReasonAlreadyArchived:  "template is already archived",
	templateReasonAlreadyExists:    "a template with this name and version already exists",
	templateReasonNoFieldsToUpdate: "no fields to update",
	templateReasonInvalidSections:  "sections could not be decoded as a valid section array",
}

// ListTemplates returns all templates in every state.
func (s *Service) ListTemplates(
	ctx context.Context, _ *svc.ListTemplatesPayload,
) ([]*svc.AdminTemplate, error) {
	templates, err := s.templates.List(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "formationService.list-templates", log.ErrKey, err)
		return nil, err
	}
	out := make([]*svc.AdminTemplate, len(templates))
	for i, t := range templates {
		out[i] = templateToWire(t)
	}
	return out, nil
}

// GetTemplate returns one template by UID.
func (s *Service) GetTemplate(
	ctx context.Context, p *svc.GetTemplatePayload,
) (*svc.AdminTemplate, error) {
	uid, err := uuid.Parse(p.UID)
	if err != nil {
		return nil, mapTemplateError(domain.NewReasonError(domain.ErrNotFound, templateReasonNotFound))
	}

	t, err := s.templates.Get(ctx, uid)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, mapTemplateError(domain.NewReasonError(domain.ErrNotFound, templateReasonNotFound))
		}
		slog.ErrorContext(ctx, "formationService.get-template", "template_uid", p.UID, log.ErrKey, err)
		return nil, err
	}
	return templateToWire(t), nil
}

// CreateTemplate creates a new draft template.
func (s *Service) CreateTemplate(
	ctx context.Context, p *svc.CreateTemplatePayload,
) (*svc.AdminTemplate, error) {
	sections, err := parseSections(p.Sections)
	if err != nil {
		return nil, mapTemplateError(domain.NewReasonErrorf(domain.ErrInvalidRequest, templateReasonInvalidSections,
			"sections: %v", err))
	}

	t := &model.Template{
		Name:     p.Name,
		Version:  p.TemplateVersion,
		State:    model.TemplateDraft,
		Priority: p.Priority,
		Match:    p.Match,
		Sections: sections,
	}
	if p.Author != nil {
		t.Author = *p.Author
	}

	created, err := s.templates.Create(ctx, t)
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil, mapTemplateError(domain.NewReasonError(domain.ErrConflict, templateReasonAlreadyExists))
		}
		slog.ErrorContext(ctx, "formationService.create-template", log.ErrKey, err)
		return nil, err
	}

	slog.InfoContext(ctx, "formationService.create-template",
		"template_uid", created.UID,
		"name", created.Name,
		"version", created.Version,
	)
	return templateToWire(created), nil
}

// UpdateTemplate applies mutable fields to a draft template.
func (s *Service) UpdateTemplate(
	ctx context.Context, p *svc.UpdateTemplatePayload,
) (*svc.AdminTemplate, error) {
	uid, err := uuid.Parse(p.UID)
	if err != nil {
		return nil, mapTemplateError(domain.NewReasonError(domain.ErrNotFound, templateReasonNotFound))
	}

	patch := port.TemplatePatch{
		Priority: p.Priority,
		Match:    p.Match,
		Author:   p.Author,
	}
	if p.Sections != nil {
		sections, err := parseSections(p.Sections)
		if err != nil {
			return nil, mapTemplateError(domain.NewReasonErrorf(domain.ErrInvalidRequest, templateReasonInvalidSections,
				"sections: %v", err))
		}
		patch.Sections = &sections
	}

	updated, err := s.templates.Update(ctx, uid, patch)
	if err != nil {
		return nil, mapTemplateError(withTemplateReason(err))
	}

	slog.InfoContext(ctx, "formationService.update-template", "template_uid", uid)
	return templateToWire(updated), nil
}

// PublishTemplate transitions a draft template to published.
func (s *Service) PublishTemplate(
	ctx context.Context, p *svc.PublishTemplatePayload,
) (*svc.AdminTemplate, error) {
	uid, err := uuid.Parse(p.UID)
	if err != nil {
		return nil, mapTemplateError(domain.NewReasonError(domain.ErrNotFound, templateReasonNotFound))
	}

	published, err := s.templates.Publish(ctx, uid)
	if err != nil {
		return nil, mapTemplateError(withTemplateReason(err))
	}

	slog.InfoContext(ctx, "formationService.publish-template", "template_uid", uid)
	return templateToWire(published), nil
}

// ArchiveTemplate transitions a template to archived.
func (s *Service) ArchiveTemplate(
	ctx context.Context, p *svc.ArchiveTemplatePayload,
) (*svc.AdminTemplate, error) {
	uid, err := uuid.Parse(p.UID)
	if err != nil {
		return nil, mapTemplateError(domain.NewReasonError(domain.ErrNotFound, templateReasonNotFound))
	}

	archived, err := s.templates.Archive(ctx, uid)
	if err != nil {
		return nil, mapTemplateError(withTemplateReason(err))
	}

	slog.InfoContext(ctx, "formationService.archive-template", "template_uid", uid)
	return templateToWire(archived), nil
}

// templateToWire converts a model.Template to the wire type.
func templateToWire(t *model.Template) *svc.AdminTemplate {
	w := &svc.AdminTemplate{
		UID:             t.UID.String(),
		Name:            t.Name,
		TemplateVersion: t.Version,
		State:           string(t.State),
		Priority:        t.Priority,
		Match:           t.Match,
		Sections:        t.Sections,
		CreatedAt:       t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:       t.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if t.Author != "" {
		w.Author = &t.Author
	}
	if t.PublishedAt != nil {
		s := t.PublishedAt.UTC().Format(time.RFC3339)
		w.PublishedAt = &s
	}
	return w
}

// parseSections decodes the wire sections value into the domain type. The
// sections arrive as interface{} (decoded from JSON by Goa) and need to be
// round-tripped through JSON to produce the typed slice.
func parseSections(raw any) ([]model.TemplateSection, error) {
	if raw == nil {
		return []model.TemplateSection{}, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encoding sections: %w", err)
	}
	var sections []model.TemplateSection
	if err := json.Unmarshal(b, &sections); err != nil {
		return nil, fmt.Errorf("sections must be an array of section objects: %w", err)
	}
	if err := validateSections(sections); err != nil {
		return nil, err
	}
	return sections, nil
}

// validateSections checks that each section and item carries the non-empty keys
// that the expander requires. An empty key would silently produce items with
// no identity, making them unresolvable in downstream checklist reads.
func validateSections(sections []model.TemplateSection) error {
	for i, s := range sections {
		if s.Key == "" {
			return fmt.Errorf("section[%d]: key is required", i)
		}
		for j, item := range s.Items {
			if item.Key == "" {
				return fmt.Errorf("section[%d].items[%d]: key is required", i, j)
			}
		}
	}
	return nil
}

// withTemplateReason wraps bare domain sentinel errors with a machine-readable
// reason so mapTemplateError can pick the right HTTP status and reason code.
// Adapters that return domain.NewReasonError pass straight through; bare
// sentinels get a default reason added.
func withTemplateReason(err error) error {
	if err == nil {
		return nil
	}
	var re *domain.ReasonError
	if errors.As(err, &re) {
		return err // already carries a typed reason from the adapter
	}
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return domain.NewReasonError(domain.ErrNotFound, templateReasonNotFound)
	case errors.Is(err, domain.ErrConflict):
		return domain.NewReasonError(domain.ErrConflict, templateReasonNotDraft)
	case errors.Is(err, domain.ErrInvalidRequest):
		return domain.NewReasonError(domain.ErrInvalidRequest, templateReasonNoFieldsToUpdate)
	}
	return err
}

// mapTemplateError converts a domain refusal into the declared TemplateError.
func mapTemplateError(err error) error {
	var re *domain.ReasonError
	if !errors.As(err, &re) {
		return err
	}

	message := re.Message
	if message == "" {
		message = templateReasonMessages[re.Reason]
	}
	if message == "" {
		message = re.Err.Error()
	}

	var name, code string
	switch {
	case errors.Is(re.Err, domain.ErrNotFound):
		name, code = "NotFound", "404"
	case errors.Is(re.Err, domain.ErrConflict):
		name, code = "Conflict", "409"
	case errors.Is(re.Err, domain.ErrInvalidRequest):
		name, code = "BadRequest", "400"
	default:
		return err
	}

	return &svc.TemplateError{Name: name, Code: code, Message: message, Reason: re.Reason}
}
