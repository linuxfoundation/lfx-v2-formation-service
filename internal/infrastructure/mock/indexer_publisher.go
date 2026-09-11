// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"sync"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// capture records values of one kind published to it, guarding them with its
// own lock so IndexerPublisher's two capture kinds (formation projections and
// item projections) do not need to repeat the same lock/copy/clone discipline
// twice.
type capture[T any] struct {
	mu    sync.Mutex
	items []T
}

// add records value.
func (c *capture[T]) add(value T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = append(c.items, value)
}

// all returns every value recorded so far, in order, cloned so a caller
// holding the result cannot see a later add mutate it out from under them.
func (c *capture[T]) all() []T {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]T{}, c.items...)
}

// count reports how many values were recorded.
func (c *capture[T]) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// latest returns the most recent value matching pred, or the zero value and
// false if none match. Most recent rather than first, because the sweep
// republishes and a test checking the effect of a change wants the state
// after it.
func (c *capture[T]) latest(pred func(T) bool) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := len(c.items) - 1; i >= 0; i-- {
		if pred(c.items[i]) {
			return c.items[i], true
		}
	}
	var zero T
	return zero, false
}

// IndexerPublisher captures the projections published to it, so a test can
// assert on what the queue would show rather than on whether a publish happened.
type IndexerPublisher struct {
	mu sync.Mutex
	// err, when set, is returned by every publish/delete method. Guarded by mu
	// rather than the capture types' own locks, since it is read and written
	// independently of any one capture.
	err error
	// itemsErr, when set, is returned by PublishItems only — separately from
	// err, so a test can simulate the checklist row publishing while every
	// item on it fails, which SetError's single blanket switch cannot express
	// (it fails PublishFormation too, before PublishItems is ever reached).
	itemsErr error

	published     capture[*port.FormationProjection]
	deleted       capture[string]
	itemPublished capture[*port.ItemProjection]
	itemDeleted   capture[string]
}

// NewIndexerPublisher constructs an empty capture.
func NewIndexerPublisher() *IndexerPublisher {
	return &IndexerPublisher{}
}

// SetError makes every subsequent publish fail, so a test can check that a
// failed projection does not fail the operation that triggered it.
func (p *IndexerPublisher) SetError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

// SetItemsError makes every subsequent PublishItems call fail, without
// affecting PublishFormation or a direct PublishItem call. See itemsErr.
func (p *IndexerPublisher) SetItemsError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.itemsErr = err
}

func (p *IndexerPublisher) armedError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *IndexerPublisher) armedItemsError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.itemsErr
}

// PublishFormation records the projection, or fails if SetError armed an error.
func (p *IndexerPublisher) PublishFormation(_ context.Context, doc *port.FormationProjection) error {
	if err := p.armedError(); err != nil {
		return err
	}
	// Stored by value behind a fresh pointer: the caller builds these per sweep
	// and a test asserting on an earlier one must not see a later mutation.
	copied := *doc
	p.published.add(&copied)
	return nil
}

// DeleteFormation records the removal, or fails if SetError armed an error.
//
// Recorded separately from the publishes rather than as an absence among them.
// A test for the repair job is asking "was this row removed", and inferring
// that from a projection that is missing from a list cannot tell a row that was
// deleted from one that was never published.
func (p *IndexerPublisher) DeleteFormation(_ context.Context, formationUID string) error {
	if err := p.armedError(); err != nil {
		return err
	}
	p.deleted.add(formationUID)
	return nil
}

// PublishItem records the item projection, or fails if SetError armed an
// error. Mirrors PublishFormation exactly.
func (p *IndexerPublisher) PublishItem(_ context.Context, doc *port.ItemProjection) error {
	if err := p.armedError(); err != nil {
		return err
	}
	copied := *doc
	p.itemPublished.add(&copied)
	return nil
}

// PublishItems records every item projection, or fails the whole batch if
// SetError or SetItemsError armed an error.
func (p *IndexerPublisher) PublishItems(ctx context.Context, docs []*port.ItemProjection) error {
	if err := p.armedItemsError(); err != nil {
		return err
	}
	for _, doc := range docs {
		if err := p.PublishItem(ctx, doc); err != nil {
			return err
		}
	}
	return nil
}

// DeleteItem records the removal, or fails if SetError armed an error.
// Mirrors DeleteFormation exactly, including recording separately from the
// publishes rather than as an absence among them.
func (p *IndexerPublisher) DeleteItem(_ context.Context, itemUID string) error {
	if err := p.armedError(); err != nil {
		return err
	}
	p.itemDeleted.add(itemUID)
	return nil
}

// Deleted returns the formation UIDs removed so far, in order.
func (p *IndexerPublisher) Deleted() []string {
	return p.deleted.all()
}

// Published returns every projection published so far, in order.
func (p *IndexerPublisher) Published() []*port.FormationProjection {
	return p.published.all()
}

// Latest returns the most recent projection for a project, or nil.
func (p *IndexerPublisher) Latest(projectUID string) *port.FormationProjection {
	doc, ok := p.published.latest(func(d *port.FormationProjection) bool {
		return d.ProjectUID == projectUID
	})
	if !ok {
		return nil
	}
	return doc
}

// Count reports how many publishes were made, which is how a test checks the
// sweep publishes once per checklist rather than once per item.
func (p *IndexerPublisher) Count() int {
	return p.published.count()
}

// ItemDeleted returns the item UIDs removed so far, in order.
func (p *IndexerPublisher) ItemDeleted() []string {
	return p.itemDeleted.all()
}

// ItemPublished returns every item projection published so far, in order.
func (p *IndexerPublisher) ItemPublished() []*port.ItemProjection {
	return p.itemPublished.all()
}

// LatestItem returns the most recent projection for an item, or nil.
func (p *IndexerPublisher) LatestItem(itemUID string) *port.ItemProjection {
	doc, ok := p.itemPublished.latest(func(d *port.ItemProjection) bool {
		return d.ItemUID == itemUID
	})
	if !ok {
		return nil
	}
	return doc
}

// ItemCount reports how many item publishes were made, which is how a test
// checks the sweep publishes one item document per item.
func (p *IndexerPublisher) ItemCount() int {
	return p.itemPublished.count()
}
