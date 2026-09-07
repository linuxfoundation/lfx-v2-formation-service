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

// TestUnitOfWorkAtomicity is FR-024/FR-025's test: a failure between the item
// write and the activity write must persist neither. The failure is induced
// by appending an activity entry that references a formation_uid the
// database has never seen, which the foreign key refuses — that refusal must
// roll back the item update in the same call.
func TestUnitOfWorkAtomicity(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	uow := NewUnitOfWork(db)

	templateRepo := NewTemplateRepo(db)
	template, err := templateRepo.Upsert(ctx, &model.Template{
		Name:     "tx-test-template",
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
		ProjectUID:      "tx-test-project",
		TemplateUID:     template.UID,
		TemplateVersion: template.Version,
	})
	if err != nil {
		t.Fatalf("seed formation: %v", err)
	}

	itemRepo := NewItemRepo(db)
	item := &model.Item{
		FormationUID: formation.UID,
		ItemKey:      "tx_test_item",
		SectionKey:   "legal_and_entity",
		Position:     1,
		Title:        "TX test item",
		Status:       model.StatusNotStarted,
	}
	if err := itemRepo.InsertMany(ctx, []*model.Item{item}); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	inProgress := model.StatusInProgress
	missingFormation := uuid.New() // never inserted; the FK on formation_activity must refuse it

	err = uow.Do(ctx, func(tx port.Tx) error {
		if _, err := tx.Items().Update(ctx, item.UID, item.Revision, port.ItemPatch{
			Status: &inProgress,
		}); err != nil {
			return err
		}
		return tx.Activity().Append(ctx, &model.ActivityEntry{
			ULID:         "01ARZ3NDEKTSV4RRFFQ69G5FAV",
			FormationUID: missingFormation,
			Actor:        "tester",
			SetBy:        model.SetByUser,
			Action:       "status_changed",
		})
	})
	if err == nil {
		t.Fatal("expected the unit of work to fail on the activity foreign key, got nil")
	}

	got, err := itemRepo.Get(ctx, item.UID)
	if err != nil {
		t.Fatalf("re-fetch item: %v", err)
	}
	if got.Status != model.StatusNotStarted {
		t.Errorf("item status: got %q, want %q (the failed activity append must have rolled back the item update)", got.Status, model.StatusNotStarted)
	}
	if got.Revision != item.Revision {
		t.Errorf("item revision: got %d, want %d (unchanged)", got.Revision, item.Revision)
	}

	var count int
	if err := db.NewSelect().
		Model((*model.ActivityEntry)(nil)).
		Where("formation_uid = ?", formation.UID).
		ColumnExpr("count(*)").
		Scan(ctx, &count); err != nil {
		t.Fatalf("count activity: %v", err)
	}
	if count != 0 {
		t.Errorf("activity rows for formation: got %d, want 0 (an entry with no change must be impossible)", count)
	}
}

// TestUnitOfWorkCommitsBothTogether is the positive case: when both writes
// succeed, both are visible after commit.
func TestUnitOfWorkCommitsBothTogether(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	uow := NewUnitOfWork(db)

	templateRepo := NewTemplateRepo(db)
	template, err := templateRepo.Upsert(ctx, &model.Template{
		Name:     "tx-test-template-commit",
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
		ProjectUID:      "tx-test-project-commit",
		TemplateUID:     template.UID,
		TemplateVersion: template.Version,
	})
	if err != nil {
		t.Fatalf("seed formation: %v", err)
	}

	itemRepo := NewItemRepo(db)
	item := &model.Item{
		FormationUID: formation.UID,
		ItemKey:      "tx_test_item_commit",
		SectionKey:   "legal_and_entity",
		Position:     1,
		Title:        "TX test item",
		Status:       model.StatusNotStarted,
	}
	if err := itemRepo.InsertMany(ctx, []*model.Item{item}); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	inProgress := model.StatusInProgress
	err = uow.Do(ctx, func(tx port.Tx) error {
		if _, err := tx.Items().Update(ctx, item.UID, item.Revision, port.ItemPatch{
			Status: &inProgress,
		}); err != nil {
			return err
		}
		return tx.Activity().Append(ctx, &model.ActivityEntry{
			ULID:         "01ARZ3NDEKTSV4RRFFQ69G5FAW",
			FormationUID: formation.UID,
			ItemUID:      &item.UID,
			Actor:        "tester",
			SetBy:        model.SetByUser,
			Action:       "status_changed",
		})
	})
	if err != nil {
		t.Fatalf("unit of work: %v", err)
	}

	got, err := itemRepo.Get(ctx, item.UID)
	if err != nil {
		t.Fatalf("re-fetch item: %v", err)
	}
	if got.Status != model.StatusInProgress {
		t.Errorf("item status: got %q, want %q", got.Status, model.StatusInProgress)
	}

	entries, _, err := NewActivityRepo(db).List(ctx, formation.UID, "", 10)
	if err != nil {
		t.Fatalf("list activity: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("activity entries: got %d, want 1", len(entries))
	}
}
