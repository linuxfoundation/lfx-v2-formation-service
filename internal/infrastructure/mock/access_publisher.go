// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"sync"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// AccessPublisher records the grants a use case asked for, so a test can
// assert on who was granted what without a broker.
//
// Captures rather than counts. The thing worth asserting about a grant is not
// that one was published but that it named the submitter and the team and
// nobody wider, and a counter cannot answer that.
type AccessPublisher struct {
	mu        sync.Mutex
	published []port.ApplicationAccess
	deleted   []string

	// Err, when set, fails every publish. Used to prove the create path
	// survives a failed grant rather than rolling the record back.
	Err error
}

var _ port.AccessPublisher = (*AccessPublisher)(nil)

// NewAccessPublisher returns an empty capture.
func NewAccessPublisher() *AccessPublisher {
	return &AccessPublisher{}
}

// PublishApplicationAccess records one grant set.
func (p *AccessPublisher) PublishApplicationAccess(_ context.Context, access port.ApplicationAccess) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return p.Err
	}
	p.published = append(p.published, access)
	return nil
}

// DeleteApplicationAccess records one revocation.
func (p *AccessPublisher) DeleteApplicationAccess(_ context.Context, applicationUID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return p.Err
	}
	p.deleted = append(p.deleted, applicationUID)
	return nil
}

// Published returns every grant set published, in order.
func (p *AccessPublisher) Published() []port.ApplicationAccess {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]port.ApplicationAccess(nil), p.published...)
}

// Deleted returns every application UID revoked, in order.
func (p *AccessPublisher) Deleted() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.deleted...)
}
