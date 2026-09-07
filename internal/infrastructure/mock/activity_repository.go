// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// ActivityRepository is an in-memory port.ActivityRepository double. There is
// no update or delete method, matching the immutability the real repository
// enforces by omission.
type ActivityRepository struct {
	mu      sync.Mutex
	entries []*model.ActivityEntry
}

// NewActivityRepository constructs an empty double.
func NewActivityRepository() *ActivityRepository {
	return &ActivityRepository{}
}

// Append adds one entry, assigning a ULID when the caller left it empty.
func (r *ActivityRepository) Append(_ context.Context, e *model.ActivityEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	clone := *e
	if clone.ULID == "" {
		clone.ULID = ulid.Make().String()
	}
	r.entries = append(r.entries, &clone)
	return nil
}

// List returns entries newest-first for a formation, honoring cursor and
// limit. cursor is the ULID of the last entry from the previous page.
func (r *ActivityRepository) List(_ context.Context, formationUID uuid.UUID, cursor string, limit int) ([]*model.ActivityEntry, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	matching := make([]*model.ActivityEntry, 0)
	for i := len(r.entries) - 1; i >= 0; i-- {
		if r.entries[i].FormationUID == formationUID {
			matching = append(matching, r.entries[i])
		}
	}

	start := 0
	if cursor != "" {
		for i, e := range matching {
			if e.ULID == cursor {
				start = i + 1
				break
			}
		}
	}

	end := start + limit
	if limit <= 0 || end > len(matching) {
		end = len(matching)
	}
	if start > len(matching) {
		start = len(matching)
	}

	page := make([]*model.ActivityEntry, 0, end-start)
	for _, e := range matching[start:end] {
		clone := *e
		page = append(page, &clone)
	}

	nextCursor := ""
	if end < len(matching) {
		nextCursor = matching[end-1].ULID
	}
	return page, nextCursor, nil
}
