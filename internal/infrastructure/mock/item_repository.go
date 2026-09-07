// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// dueDateLayout matches the "date" column type patch.DueDate is stored
// against. The real repository never parses this itself — it passes the
// string straight to Postgres and lets the driver cast it — so this double
// has to do the cast Postgres would have done.
const dueDateLayout = "2006-01-02"

// ItemRepository is an in-memory port.ItemRepository double.
type ItemRepository struct {
	mu    sync.Mutex
	items map[uuid.UUID]*model.Item
}

// NewItemRepository constructs an empty double.
func NewItemRepository() *ItemRepository {
	return &ItemRepository{items: make(map[uuid.UUID]*model.Item)}
}

// InsertMany expands a checklist. Items already present by (formation, key)
// are left untouched, matching the real repository's idempotent-expansion
// contract.
func (r *ItemRepository) InsertMany(_ context.Context, items []*model.Item) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, item := range items {
		if r.hasKeyLocked(item.FormationUID, item.ItemKey) {
			continue
		}
		clone := *item
		if clone.UID == uuid.Nil {
			clone.UID = uuid.New()
		}
		if clone.Status == "" {
			clone.Status = model.StatusNotStarted
		}
		clone.Revision = 1
		r.items[clone.UID] = &clone
	}
	return nil
}

func (r *ItemRepository) hasKeyLocked(formationUID uuid.UUID, itemKey string) bool {
	for _, item := range r.items {
		if item.FormationUID == formationUID && item.ItemKey == itemKey {
			return true
		}
	}
	return false
}

// ListByFormation returns every item for a formation. Ordering by section
// then position is not reproduced here — business-rule tests using this
// double should not depend on row order.
func (r *ItemRepository) ListByFormation(_ context.Context, formationUID uuid.UUID) ([]*model.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*model.Item, 0)
	for _, item := range r.items {
		if item.FormationUID == formationUID {
			clone := *item
			out = append(out, &clone)
		}
	}
	return out, nil
}

// Get returns one item by UID, or domain.ErrNotFound.
func (r *ItemRepository) Get(_ context.Context, uid uuid.UUID) (*model.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	item, ok := r.items[uid]
	if !ok {
		return nil, domain.ErrNotFound
	}
	out := *item
	return &out, nil
}

// Update applies patch's non-nil fields, refusing a stale revision with
// domain.ErrVersionMismatch.
func (r *ItemRepository) Update(_ context.Context, uid uuid.UUID, revision int64, patch port.ItemPatch) (*model.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	item, ok := r.items[uid]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if item.Revision != revision {
		return nil, domain.ErrVersionMismatch
	}

	if patch.DueDate != nil {
		due, err := time.Parse(dueDateLayout, *patch.DueDate)
		if err != nil {
			return nil, fmt.Errorf("mock item repository: parse due_date %q: %w", *patch.DueDate, err)
		}
		item.DueDate = &due
	}

	if patch.Status != nil {
		item.Status = *patch.Status
	}
	if patch.Assignee != nil {
		item.Assignee = *patch.Assignee
	}
	if patch.Note != nil {
		item.Note = *patch.Note
	}
	if patch.SkipReason != nil {
		item.SkipReason = *patch.SkipReason
	}
	if patch.EvidenceLink != nil {
		item.EvidenceLink = *patch.EvidenceLink
	}
	if patch.ResolvedRef != nil {
		item.ResolvedRef = patch.ResolvedRef
	}
	if patch.SubItems != nil {
		item.SubItems = *patch.SubItems
	}
	item.Revision++

	out := *item
	return &out, nil
}

// StatusCounts returns one count per status.
func (r *ItemRepository) StatusCounts(_ context.Context, formationUID uuid.UUID) (map[model.ItemStatus]int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	counts := make(map[model.ItemStatus]int, len(model.AllItemStatuses))
	for _, s := range model.AllItemStatuses {
		counts[s] = 0
	}
	for _, item := range r.items {
		if item.FormationUID == formationUID {
			counts[item.Status]++
		}
	}
	return counts, nil
}

// GateSummary reports how many gating items exist and how many are not done.
func (r *ItemRepository) GateSummary(_ context.Context, formationUID uuid.UUID) (total int, outstanding int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, item := range r.items {
		if item.FormationUID != formationUID || !item.Gate {
			continue
		}
		total++
		if item.Status != model.StatusDone {
			outstanding++
		}
	}
	return total, outstanding, nil
}
