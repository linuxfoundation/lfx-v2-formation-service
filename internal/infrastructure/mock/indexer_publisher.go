// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"sync"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// IndexerPublisher captures the projections published to it, so a test can
// assert on what the queue would show rather than on whether a publish happened.
type IndexerPublisher struct {
	mu        sync.Mutex
	published []*port.FormationProjection
	deleted   []string
	err       error
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

// PublishFormation records the projection, or fails if SetError armed an error.
func (p *IndexerPublisher) PublishFormation(_ context.Context, doc *port.FormationProjection) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.err != nil {
		return p.err
	}
	// Stored by value behind a fresh pointer: the caller builds these per sweep
	// and a test asserting on an earlier one must not see a later mutation.
	copied := *doc
	p.published = append(p.published, &copied)
	return nil
}

// DeleteFormation records the removal, or fails if SetError armed an error.
//
// Recorded separately from the publishes rather than as an absence among them.
// A test for the repair job is asking "was this row removed", and inferring
// that from a projection that is missing from a list cannot tell a row that was
// deleted from one that was never published.
func (p *IndexerPublisher) DeleteFormation(_ context.Context, formationUID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.err != nil {
		return p.err
	}
	p.deleted = append(p.deleted, formationUID)
	return nil
}

// Deleted returns the formation UIDs removed so far, in order.
func (p *IndexerPublisher) Deleted() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.deleted...)
}

// Published returns every projection published so far, in order.
func (p *IndexerPublisher) Published() []*port.FormationProjection {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*port.FormationProjection{}, p.published...)
}

// Latest returns the most recent projection for a project, or nil.
//
// Most recent rather than first, because the sweep republishes and a test
// checking the effect of a change wants the state after it.
func (p *IndexerPublisher) Latest(projectUID string) *port.FormationProjection {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := len(p.published) - 1; i >= 0; i-- {
		if p.published[i].ProjectUID == projectUID {
			return p.published[i]
		}
	}
	return nil
}

// Count reports how many publishes were made, which is how a test checks the
// sweep publishes once per checklist rather than once per item.
func (p *IndexerPublisher) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.published)
}
