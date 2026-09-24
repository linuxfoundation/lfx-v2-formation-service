// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// UnitOfWork is an in-memory port.UnitOfWork double. It runs fn directly
// against the same repository instances the rest of REPOSITORY_SOURCE=mock
// wiring uses, rather than against a copy — so a mutation made inside Do is
// visible to a later read the same way the Postgres implementation's
// committed transaction is. There is no real atomicity: each mock repository
// already guards its own state with a mutex, and mock mode is for local
// development and tests, not for exercising rollback behavior (that is
// postgres/unit_of_work_test.go's job, against a real transaction).
type UnitOfWork struct {
	Recorder
	tx tx
}

// NewUnitOfWork wires a unit of work over the given repository doubles.
func NewUnitOfWork(
	formations *FormationRepository,
	items *ItemRepository,
	activity *ActivityRepository,
	templates *TemplateRepository,
	applications *ApplicationRepository,
) *UnitOfWork {
	return &UnitOfWork{tx: tx{
		formations:   formations,
		items:        items,
		activity:     activity,
		templates:    templates,
		applications: applications,
	}}
}

// Do runs fn against the wired repositories and preserves the production
// adapter's error chain.
func (u *UnitOfWork) Do(_ context.Context, fn func(port.Tx) error) error {
	u.record("uow.Do")
	if err := fn(u.tx); err != nil {
		return fmt.Errorf("unit of work: %w", err)
	}
	return nil
}

// tx implements port.Tx by returning the same repository instances the
// UnitOfWork was constructed with.
type tx struct {
	formations   *FormationRepository
	items        *ItemRepository
	activity     *ActivityRepository
	templates    *TemplateRepository
	applications *ApplicationRepository
}

func (t tx) Formations() port.FormationRepository     { return t.formations }
func (t tx) Items() port.ItemRepository               { return t.items }
func (t tx) Activity() port.ActivityRepository        { return t.activity }
func (t tx) Templates() port.TemplateRepository       { return t.templates }
func (t tx) Applications() port.ApplicationRepository { return t.applications }
