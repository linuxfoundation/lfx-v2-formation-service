// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"

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
	if _, err := itemRepo.InsertMany(ctx, []*model.Item{item}); err != nil {
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
	if _, err := NewItemRepo(db).InsertMany(ctx, []*model.Item{item}); err != nil {
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
	if _, err := NewItemRepo(db).InsertMany(ctx, []*model.Item{resolved}); err != nil {
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

// InsertMany's return value is the audit trail's source of truth for what an
// upgrade added, so it has to name the rows this statement actually wrote. A
// conflicting row was written by somebody else, and a caller that recorded its
// own input instead would claim credit for it.
//
// The generated UIDs matter just as much: RETURNING yields nothing for a
// suppressed row, so the result stops lining up positionally with the input and
// scanning it straight back into the models would attach one item's UID to
// another.
func TestInsertManyReportsOnlyTheRowsItWrote(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	templateRepo := NewTemplateRepo(db)
	template, err := templateRepo.Upsert(ctx, &model.Template{
		Name:     "insert-many-template",
		Version:  1,
		State:    model.TemplatePublished,
		Priority: 100,
		Match:    "always",
		Sections: []model.TemplateSection{},
	})
	if err != nil {
		t.Fatalf("seed template: %v", err)
	}

	formation, err := NewFormationRepo(db).Create(ctx, &model.Formation{
		ProjectUID:      "insert-many-project",
		TemplateUID:     template.UID,
		TemplateVersion: template.Version,
	})
	if err != nil {
		t.Fatalf("seed formation: %v", err)
	}

	newItem := func(key string) *model.Item {
		return &model.Item{
			FormationUID: formation.UID,
			ItemKey:      key,
			SectionKey:   "legal_and_entity",
			Title:        key,
		}
	}

	itemRepo := NewItemRepo(db)
	first, err := itemRepo.InsertMany(ctx, []*model.Item{newItem("already_there")})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if len(first) != 1 || first[0] != "already_there" {
		t.Fatalf("first insert reported %v, want [already_there]", first)
	}

	// One row conflicts, two are new: the shape a concurrent upgrade leaves.
	batch := []*model.Item{newItem("already_there"), newItem("added_one"), newItem("added_two")}
	inserted, err := itemRepo.InsertMany(ctx, batch)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}

	got := map[string]bool{}
	for _, key := range inserted {
		got[key] = true
	}
	if len(inserted) != 2 || !got["added_one"] || !got["added_two"] {
		t.Errorf("inserted = %v, want exactly the two new keys", inserted)
	}
	if got["already_there"] {
		t.Error("inserted names a row another caller had already written")
	}

	// Each new item carries its own generated UID, and the suppressed one is
	// left as the caller passed it rather than given somebody else's.
	for _, item := range batch[1:] {
		if item.UID == uuid.Nil {
			t.Errorf("%s has no UID, want the generated one", item.ItemKey)
			continue
		}
		stored, err := itemRepo.Get(ctx, item.UID)
		if err != nil {
			t.Errorf("Get(%s) = %v", item.ItemKey, err)
			continue
		}
		if stored.ItemKey != item.ItemKey {
			t.Errorf("uid for %s resolves to %s — the returned rows were misaligned",
				item.ItemKey, stored.ItemKey)
		}
	}
}
