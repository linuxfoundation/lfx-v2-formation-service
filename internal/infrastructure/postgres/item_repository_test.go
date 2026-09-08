// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// Clearing an assignee has to reach the column as SQL NULL rather than an
// empty string.
// The distinction is invisible through the model, whose Assignee is a plain
// string either way, and invisible in the mock — it is only observable here,
// which is why this test exists at the repository level.
func TestUpdateClearsAssigneeToNull(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	templateRepo := NewTemplateRepo(db)
	template, err := templateRepo.Upsert(ctx, &model.Template{
		Name:     "assignee-test-template",
		Version:  1,
		State:    model.TemplatePublished,
		Priority: 100,
		Match:    "always",
		Sections: []model.TemplateSection{},
	})
	if err != nil {
		t.Fatalf("seed template: %v", err)
	}

	formationRepo := NewFormationRepo(db)
	formation, err := formationRepo.Create(ctx, &model.Formation{
		ProjectUID:      "assignee-test-project",
		TemplateUID:     template.UID,
		TemplateVersion: template.Version,
	})
	if err != nil {
		t.Fatalf("seed formation: %v", err)
	}

	itemRepo := NewItemRepo(db)
	item := &model.Item{
		FormationUID: formation.UID,
		ItemKey:      "assignee_test_item",
		SectionKey:   "legal_and_entity",
		Position:     1,
		Title:        "Assignee test item",
		Assignee:     "someone",
	}
	if err := itemRepo.InsertMany(ctx, []*model.Item{item}); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	// Sanity: the seeded assignee is really stored, so a later NULL is the
	// clear taking effect rather than the write never having landed.
	var isNull bool
	assigneeIsNull := func() bool {
		t.Helper()
		if err := db.NewSelect().
			Table("formation_items").
			ColumnExpr("assignee IS NULL").
			Where("uid = ?", item.UID).
			Scan(ctx, &isNull); err != nil {
			t.Fatalf("read assignee: %v", err)
		}
		return isNull
	}
	if assigneeIsNull() {
		t.Fatal("seeded assignee came back NULL, so the clear below would prove nothing")
	}

	cleared := ""
	updated, err := itemRepo.Update(ctx, item.UID, item.Revision, port.ItemPatch{Assignee: &cleared})
	if err != nil {
		t.Fatalf("clear assignee: %v", err)
	}
	if updated.Assignee != "" {
		t.Errorf("model assignee: got %q, want empty", updated.Assignee)
	}
	if !assigneeIsNull() {
		t.Error("assignee stored as '' rather than NULL, which leaves the row in the partial formation_items_assignee_idx")
	}

	// The point of NULL over '': the partial index is defined WHERE assignee
	// IS NOT NULL, so a cleared row must drop out of it.
	var indexed int
	if err := db.NewSelect().
		Table("formation_items").
		ColumnExpr("count(*)").
		Where("assignee IS NOT NULL").
		Where("uid = ?", item.UID).
		Scan(ctx, &indexed); err != nil {
		t.Fatalf("count indexed rows: %v", err)
	}
	if indexed != 0 {
		t.Errorf("cleared row still matches the partial index predicate: got %d rows, want 0", indexed)
	}
}

// The insert path has to agree with the clear path about what "unassigned"
// means. Every item a checklist is created with starts unassigned, so if Bun
// writes ” here then a freshly expanded checklist puts all seventeen of its
// rows into the index of assigned work — the exact state TestUpdateClears...
// above proves the update path avoids. Only observable against a real column.
func TestInsertLeavesAnUnassignedItemNull(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	templateRepo := NewTemplateRepo(db)
	template, err := templateRepo.Upsert(ctx, &model.Template{
		Name:     "unassigned-insert-template",
		Version:  1,
		State:    model.TemplatePublished,
		Priority: 100,
		Match:    "always",
		Sections: []model.TemplateSection{},
	})
	if err != nil {
		t.Fatalf("seed template: %v", err)
	}

	formationRepo := NewFormationRepo(db)
	formation, err := formationRepo.Create(ctx, &model.Formation{
		ProjectUID:      "unassigned-insert-project",
		TemplateUID:     template.UID,
		TemplateVersion: template.Version,
	})
	if err != nil {
		t.Fatalf("seed formation: %v", err)
	}

	item := &model.Item{
		FormationUID: formation.UID,
		ItemKey:      "unassigned_item",
		SectionKey:   "legal_and_entity",
		Position:     0,
		Title:        "Unassigned item",
		// Assignee deliberately left unset, as expansion leaves it.
	}
	item.ApplyInsertDefaults()
	if err := NewItemRepo(db).InsertMany(ctx, []*model.Item{item}); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	var isNull bool
	if err := db.NewSelect().
		Table("formation_items").
		ColumnExpr("assignee IS NULL").
		Where("uid = ?", item.UID).
		Scan(ctx, &isNull); err != nil {
		t.Fatalf("read assignee: %v", err)
	}
	if !isNull {
		t.Error("a newly inserted item stored assignee as '' rather than NULL, so it sits in the partial formation_items_assignee_idx")
	}

	// requires_writer defaults to true in the schema, and Bun would send Go's
	// false over the top of it. Expansion resolves it before insert; this pins
	// that the resolved value is what lands, since a row that quietly became
	// requires_writer = false would drop the elevation prompt for that item.
	resolved := &model.Item{
		FormationUID:   formation.UID,
		ItemKey:        "writer_gated_item",
		SectionKey:     "legal_and_entity",
		Position:       1,
		Title:          "Writer gated item",
		RequiresWriter: true,
	}
	resolved.ApplyInsertDefaults()
	if err := NewItemRepo(db).InsertMany(ctx, []*model.Item{resolved}); err != nil {
		t.Fatalf("insert writer-gated item: %v", err)
	}

	var requiresWriter bool
	if err := db.NewSelect().
		Table("formation_items").
		Column("requires_writer").
		Where("uid = ?", resolved.UID).
		Scan(ctx, &requiresWriter); err != nil {
		t.Fatalf("read requires_writer: %v", err)
	}
	if !requiresWriter {
		t.Error("requires_writer stored false for an item that resolved to true")
	}
}
