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
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// templateFS carries the checklist content into the binary so seeding needs no
// file alongside the image and cannot half-apply from a missing mount.
//
//go:embed templates/project_formation_v2.json
var templateFS embed.FS

const (
	seedContentPath = "templates/project_formation_v2.json"

	// seedTemplateName and seedTemplateVersion are the Upsert key, which is
	// what makes re-running the job a no-op. Editing content without
	// bumping the version deliberately overwrites the same row: before this
	// template has ever been published that is the intent, and afterwards a
	// bump is required because live checklists pin the version they expanded
	// from.
	//
	// Version 2 differs from version 1 in one row: the repositories and
	// GitHub owner item is manual rather than platform-checked. Nothing in
	// the platform owns repositories, so no lookup can ever answer for that
	// row, and leaving it marked platform meant a permanently unanswerable
	// item that every report had to explain. A new version rather than an
	// edit because a published version is immutable, and live checklists pin
	// the version they expanded from.
	//
	// Checklists already expanded from version 1 keep it. They are corrected
	// by the migration in the schema's additive tail, which is keyed on the
	// item key rather than on the template version.
	seedTemplateName    = "Project formation"
	seedTemplateVersion = 2

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
	if len(sections) == 0 {
		return nil, fmt.Errorf("%s defines no sections", seedContentPath)
	}
	if err := model.ValidateSections(sections); err != nil {
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
