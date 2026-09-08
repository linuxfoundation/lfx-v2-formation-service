// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/service"
)

// templateFS carries the checklist content into the binary so seeding needs no
// file alongside the image and cannot half-apply from a missing mount.
//
//go:embed templates/project_formation_v1.json
var templateFS embed.FS

const (
	seedContentPath = "templates/project_formation_v1.json"

	// seedTemplateName and seedTemplateVersion are the Upsert key, which is
	// what makes re-running the job a no-op. Editing content without
	// bumping the version deliberately overwrites the same row: before this
	// template has ever been published that is the intent, and afterwards a
	// bump is required because live checklists pin the version they expanded
	// from.
	seedTemplateName    = "Project formation"
	seedTemplateVersion = 1

	// seedTemplatePriority is high because lower wins and this is the
	// fallback. A future template for a narrower kind of formation needs a
	// number below this one, and leaving a wide gap means adding it does not
	// require renumbering this row.
	seedTemplatePriority = 100

	// seedTemplateMatch is the enum-valued rule, not an expression. "always"
	// is the fallback arm, which is the only correct one for a template that
	// applies to every formation.
	seedTemplateMatch = "always"

	seedTemplateAuthor = "seed"
)

// itemKeyPattern is snake_case, enforced rather than trusted. Item keys are
// permanent: template upgrades match on key, so a rekey after any checklist
// exists is a data migration rather than an edit. Catching a stray hyphen or
// capital at seed time costs nothing; catching it later costs a migration.
var itemKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// loadSeedSections reads the embedded content. Kept separate from seeding so
// the content can be validated with no database in reach.
func loadSeedSections() ([]model.TemplateSection, error) {
	raw, err := templateFS.ReadFile(seedContentPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", seedContentPath, err)
	}

	sections, err := decodeSections(raw)
	if err != nil {
		return nil, err
	}
	if err := validateSections(sections); err != nil {
		return nil, err
	}
	return sections, nil
}

// decodeSections parses the content file. Separate from loadSeedSections so its
// refusals can be tested against content the embedded file is not allowed to
// contain.
func decodeSections(raw []byte) ([]model.TemplateSection, error) {
	var sections []model.TemplateSection
	// Unknown fields are refused: the content file is edited by hand, and a
	// misspelled "gating" silently becoming a non-gating row is exactly the
	// failure this catches.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&sections); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", seedContentPath, err)
	}
	// The file has to be exactly one document. A streaming decoder stops at the
	// end of the first value, so a second array — or anything left after a
	// truncated edit or a bad merge — would otherwise be dropped in silence and
	// the seed would report success having published only part of the template.
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing %s: expected one JSON document, found more after the first", seedContentPath)
	}
	return sections, nil
}

// validateSections enforces what the content file cannot express in its own
// syntax. Every rule here is one that costs a data migration if it reaches the
// database and is noticed later.
func validateSections(sections []model.TemplateSection) error {
	if len(sections) == 0 {
		return fmt.Errorf("%s defines no sections", seedContentPath)
	}

	// Keys are unique across the whole template, not per section: items are
	// matched on key alone at upgrade, and the checklist response is flat
	// enough that two rows sharing a key would collide there too.
	seenItem := make(map[string]string)
	seenSection := make(map[string]bool)

	for _, section := range sections {
		if !itemKeyPattern.MatchString(section.Key) {
			return fmt.Errorf("section key %q is not snake_case", section.Key)
		}
		if seenSection[section.Key] {
			return fmt.Errorf("section key %q appears twice", section.Key)
		}
		seenSection[section.Key] = true

		if section.Title == "" {
			return fmt.Errorf("section %q has no title", section.Key)
		}

		if len(section.Items) == 0 {
			return fmt.Errorf("section %q defines no items", section.Key)
		}

		for _, item := range section.Items {
			if !itemKeyPattern.MatchString(item.Key) {
				return fmt.Errorf("item key %q is not snake_case", item.Key)
			}
			if other, dup := seenItem[item.Key]; dup {
				return fmt.Errorf("item key %q appears in both %q and %q", item.Key, other, section.Key)
			}
			seenItem[item.Key] = section.Key

			if item.Title == "" {
				return fmt.Errorf("item %q has no title", item.Key)
			}
			if err := validateItemSource(item); err != nil {
				return err
			}
			if err := validateItemDisplay(item); err != nil {
				return err
			}

			// Sub-item keys are unique within their item, which is the scope
			// a status patch addresses them in.
			seenSub := make(map[string]bool, len(item.SubItems))
			for _, sub := range item.SubItems {
				if !itemKeyPattern.MatchString(sub.Key) {
					return fmt.Errorf("sub-item key %q is not snake_case", sub.Key)
				}
				if sub.Title == "" {
					return fmt.Errorf("sub-item %q has no title", sub.Key)
				}
				if seenSub[sub.Key] {
					return fmt.Errorf("item %q has sub-item key %q twice", item.Key, sub.Key)
				}
				seenSub[sub.Key] = true
			}
		}
	}
	return nil
}

// validateItemSource keeps status_source and platform_check consistent. The two
// are one decision expressed in two fields, and either half alone is a silent
// misconfiguration: a platform row with no check never resolves, and a check on
// a manual row is dead weight that reads as automation to anyone auditing it.
func validateItemSource(item model.TemplateItem) error {
	switch item.StatusSource {
	case model.SourcePlatform:
		if item.PlatformCheck == nil {
			return fmt.Errorf("item %q is platform-sourced with no platform_check", item.Key)
		}
		if item.PlatformCheck.ResourceType == "" {
			return fmt.Errorf("item %q has a platform_check with no resource_type", item.Key)
		}
		if item.PlatformCheck.MinCount < 1 {
			return fmt.Errorf("item %q has a platform_check with min_count %d, which nothing can fail",
				item.Key, item.PlatformCheck.MinCount)
		}
	case model.SourceManual:
		if item.PlatformCheck != nil {
			return fmt.Errorf("item %q is manual but carries a platform_check", item.Key)
		}
	default:
		return fmt.Errorf("item %q has status_source %q, want %q or %q",
			item.Key, item.StatusSource, model.SourceManual, model.SourcePlatform)
	}
	return nil
}

// validateItemDisplay checks the two fields that are copied to the row verbatim
// and never corrected downstream, so a bad value here reaches a reader.
//
// checklist_type is the sharper of the two: it is copied straight through, the
// insert defaulting only fills it when empty, and the checklist response
// declares it as an enum — so a typo ships and then fails every read of that
// checklist with a 500 rather than failing here.
func validateItemDisplay(item model.TemplateItem) error {
	switch item.ChecklistType {
	case "", model.ChecklistInternal, model.ChecklistExternal, model.ChecklistBoth:
	default:
		return fmt.Errorf("item %q has checklist_type %q, want %q, %q or %q",
			item.Key, item.ChecklistType,
			model.ChecklistInternal, model.ChecklistExternal, model.ChecklistBoth)
	}

	// An unparseable due rule is silent in a way the others are not: expansion
	// warns and carries on, so the checklist ships with no due date and nothing
	// on the screen says why.
	if item.DueRule != "" {
		if _, ok := service.ParseDueRule(item.DueRule); !ok {
			return fmt.Errorf("item %q has due_rule %q, want the form announcement-<n>d", item.Key, item.DueRule)
		}
	}
	return nil
}

// buildSeedTemplate assembles the published template from the loaded content.
func buildSeedTemplate(sections []model.TemplateSection, now time.Time) *model.Template {
	published := now.UTC()
	tpl := &model.Template{
		Name:     seedTemplateName,
		Version:  seedTemplateVersion,
		State:    model.TemplatePublished,
		Priority: seedTemplatePriority,
		Match:    seedTemplateMatch,
		Sections: sections,
		Author:   seedTemplateAuthor,
		// Set explicitly rather than left to selection to infer: the
		// selection index is partial on state = 'published', so a template
		// seeded as a draft is invisible and the reconcile finds no
		// candidate at all.
		PublishedAt: &published,
	}
	tpl.ApplyUpsertDefaults()
	return tpl
}

// runSeed loads, validates and upserts the template. It takes the port rather
// than a connection so it can be exercised against the mock repository.
func runSeed(ctx context.Context, templates port.TemplateRepository) error {
	sections, err := loadSeedSections()
	if err != nil {
		return err
	}

	tpl := buildSeedTemplate(sections, time.Now())
	stored, err := templates.Upsert(ctx, tpl)
	if err != nil {
		return fmt.Errorf("upserting template %q v%d: %w", tpl.Name, tpl.Version, err)
	}

	slog.InfoContext(ctx, "seeded formation template",
		"uid", stored.UID,
		"name", stored.Name,
		"version", stored.Version,
		"state", stored.State,
		"sections", len(stored.Sections),
		"items", countItems(stored.Sections),
	)
	return nil
}

func countItems(sections []model.TemplateSection) int {
	n := 0
	for _, section := range sections {
		n += len(section.Items)
	}
	return n
}
