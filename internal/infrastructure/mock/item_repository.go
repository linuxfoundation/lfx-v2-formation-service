// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"fmt"
	"sort"
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
	Recorder
	mu    sync.Mutex
	items map[uuid.UUID]*model.Item
	// formations answers which checklists are live, which the Postgres
	// repository reads by joining formations. NewUnitOfWork wires it.
	formations *FormationRepository
}

// NewItemRepository constructs an empty double.
func NewItemRepository() *ItemRepository {
	return &ItemRepository{items: make(map[uuid.UUID]*model.Item)}
}

// InsertMany expands a checklist. Items already present by (formation, key)
// are left untouched, matching the real repository's idempotent-expansion
// contract, and the returned keys are the ones actually added.
func (r *ItemRepository) InsertMany(_ context.Context, items []*model.Item) ([]string, error) {
	r.record("items.InsertMany")
	r.mu.Lock()
	defer r.mu.Unlock()

	inserted := make([]string, 0, len(items))
	for _, item := range items {
		if r.hasKeyLocked(item.FormationUID, item.ItemKey) {
			continue
		}
		clone := *item
		if clone.UID == uuid.Nil {
			clone.UID = uuid.New()
		}
		clone.ApplyInsertDefaults()
		r.items[clone.UID] = &clone
		inserted = append(inserted, clone.ItemKey)
	}
	return inserted, nil
}

func (r *ItemRepository) hasKeyLocked(formationUID uuid.UUID, itemKey string) bool {
	for _, item := range r.items {
		if item.FormationUID == formationUID && item.ItemKey == itemKey {
			return true
		}
	}
	return false
}

// ListByFormation returns every item for a formation, ordered by section
// then position, matching the real repository's contract.
func (r *ItemRepository) ListByFormation(_ context.Context, formationUID uuid.UUID) ([]*model.Item, error) {
	r.record("items.ListByFormation")
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*model.Item, 0)
	for _, item := range r.items {
		if item.FormationUID == formationUID {
			clone := *item
			out = append(out, &clone)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SectionKey != out[j].SectionKey {
			return out[i].SectionKey < out[j].SectionKey
		}
		return out[i].Position < out[j].Position
	})
	return out, nil
}

// Get returns one item by UID, or domain.ErrNotFound.
func (r *ItemRepository) Get(_ context.Context, uid uuid.UUID) (*model.Item, error) {
	r.record("items.Get")
	r.mu.Lock()
	defer r.mu.Unlock()

	item, ok := r.items[uid]
	if !ok {
		return nil, domain.ErrNotFound
	}
	out := *item
	return &out, nil
}

// GetByKey returns the item at item_key within a formation, or
// domain.ErrNotFound.
func (r *ItemRepository) GetByKey(_ context.Context, formationUID uuid.UUID, itemKey string) (*model.Item, error) {
	r.record("items.GetByKey")
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, item := range r.items {
		if item.FormationUID == formationUID && item.ItemKey == itemKey {
			out := *item
			return &out, nil
		}
	}
	return nil, domain.ErrNotFound
}

// Update applies patch's non-nil fields, refusing a stale revision with
// domain.ErrVersionMismatch.
func (r *ItemRepository) Update(_ context.Context, uid uuid.UUID, revision int64, patch port.ItemPatch) (*model.Item, error) {
	r.record("items.Update")
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
		if *patch.DueDate == "" {
			// Empty string clears it, matching the real repository's
			// NULL write for the same signal.
			item.DueDate = nil
		} else {
			due, err := time.Parse(dueDateLayout, *patch.DueDate)
			if err != nil {
				return nil, fmt.Errorf("mock item repository: parse due_date %q: %w", *patch.DueDate, err)
			}
			item.DueDate = &due
		}
	}

	if patch.Status != nil {
		item.Status = *patch.Status
	}
	if patch.StatusSource != nil {
		item.StatusSource = *patch.StatusSource
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

// CountLiveRequiresWriter counts live checklists' items by key and flag,
// ordered the way the real repository orders them.
func (r *ItemRepository) CountLiveRequiresWriter(_ context.Context) ([]port.RequiresWriterCount, error) {
	r.record("items.CountLiveRequiresWriter")
	live, err := r.liveFormations()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	type group struct {
		key   string
		value bool
	}
	byGroup := make(map[group]int)
	for _, item := range r.items {
		if live[item.FormationUID] {
			byGroup[group{item.ItemKey, item.RequiresWriter}]++
		}
	}
	out := make([]port.RequiresWriterCount, 0, len(byGroup))
	for g, n := range byGroup {
		out = append(out, port.RequiresWriterCount{ItemKey: g.key, RequiresWriter: g.value, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ItemKey != out[j].ItemKey {
			return out[i].ItemKey < out[j].ItemKey
		}
		return !out[i].RequiresWriter && out[j].RequiresWriter
	})
	return out, nil
}

// SetLiveRequiresWriter sets the flag on live checklists' items with itemKey,
// leaving the revision alone as the real repository does.
func (r *ItemRepository) SetLiveRequiresWriter(_ context.Context, itemKey string, value bool) (int64, error) {
	r.record("items.SetLiveRequiresWriter")
	live, err := r.liveFormations()
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var changed int64
	for _, item := range r.items {
		if item.ItemKey == itemKey && live[item.FormationUID] && item.RequiresWriter != value {
			item.RequiresWriter = value
			changed++
		}
	}
	return changed, nil
}

// liveFormations is read before r.mu is taken, so the two repositories' locks
// are never held together.
func (r *ItemRepository) liveFormations() (map[uuid.UUID]bool, error) {
	if r.formations == nil {
		return nil, fmt.Errorf("mock item repository: no formation repository wired; build it through NewUnitOfWork")
	}
	return r.formations.liveUIDs(), nil
}
