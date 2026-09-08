// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// MatchAlways is the fallback rule: it fires for every project. It is the only
// rule the first slice defines, and the seeded template carries it.
//
// The rule vocabulary is an enumerated stand-in rather than an expression
// language, on the reasoning that the priority-ordered first-match behaviour is
// what callers depend on and the syntax is not. Adding a rule is adding a case
// to matches, which keeps an unrecognised rule a compile-time-visible gap
// instead of a silently true predicate.
const MatchAlways = "always"

// TemplateSelector picks the template a new checklist expands from.
type TemplateSelector struct {
	templates port.TemplateRepository
}

// NewTemplateSelector wires a selector over the template repository.
func NewTemplateSelector(templates port.TemplateRepository) *TemplateSelector {
	return &TemplateSelector{templates: templates}
}

// Select returns the published template with the lowest priority whose rule
// fires. Priority ordering comes from the repository, which reads it through an
// index partial on state = 'published', so a draft is not a candidate at all.
//
// It takes no project facts because no rule needs any yet. When a
// project-dependent rule lands, the facts arrive here rather than inside
// matches, so the read stays in one place and the predicate stays pure.
func (s *TemplateSelector) Select(ctx context.Context) (*model.Template, error) {
	candidates, err := s.templates.ListPublished(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing published templates: %w", err)
	}

	for _, candidate := range candidates {
		if matches(candidate.Match) {
			return candidate, nil
		}
		// Worth a log rather than silence: a template published with a rule
		// this build does not know is invisible to selection, and the symptom
		// otherwise is "no template found" with nothing naming the cause.
		slog.WarnContext(ctx, "skipping template with unrecognised match rule",
			"template_uid", candidate.UID,
			"name", candidate.Name,
			"version", candidate.Version,
			"match", candidate.Match,
		)
	}

	// Not having a template is a deployment state, not a caller mistake: the
	// seed job has not run, or every published template was skipped above.
	// ErrNotFound is right for both, and the reconcile loop's job is to retry
	// rather than to give up on the project.
	return nil, fmt.Errorf("no published template matches: %w", domain.ErrNotFound)
}

// matches evaluates one rule. An unrecognised value is false, never true: a
// template must not apply to every project merely because this build cannot
// read its rule.
func matches(rule string) bool {
	switch rule {
	case MatchAlways:
		return true
	default:
		return false
	}
}
