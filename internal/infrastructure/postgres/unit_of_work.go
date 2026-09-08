// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// UnitOfWork runs a function against repositories bound to one bun
// transaction, so an item change and its activity entry commit together or
// not at all.
type UnitOfWork struct {
	db *bun.DB
}

// NewUnitOfWork wires a unit of work over the given database.
func NewUnitOfWork(db *bun.DB) *UnitOfWork {
	return &UnitOfWork{db: db}
}

// tx implements port.Tx over a single bun.Tx, constructing repositories that
// all share the same underlying connection and transaction state.
type tx struct {
	formations *FormationRepo
	items      *ItemRepo
	activity   *ActivityRepo
	templates  *TemplateRepo
}

func (t *tx) Formations() port.FormationRepository { return t.formations }
func (t *tx) Items() port.ItemRepository           { return t.items }
func (t *tx) Activity() port.ActivityRepository    { return t.activity }
func (t *tx) Templates() port.TemplateRepository   { return t.templates }

// Do runs fn inside a transaction, committing on a nil return and rolling
// back otherwise — including on panic, since bun's RunInTx re-panics after
// rollback.
func (u *UnitOfWork) Do(ctx context.Context, fn func(port.Tx) error) error {
	err := u.db.RunInTx(ctx, nil, func(ctx context.Context, btx bun.Tx) error {
		t := &tx{
			formations: NewFormationRepo(btx),
			items:      NewItemRepo(btx),
			activity:   NewActivityRepo(btx),
			templates:  NewTemplateRepo(btx),
		}
		return fn(t)
	})
	if err != nil {
		return fmt.Errorf("unit of work: %w", err)
	}
	return nil
}
