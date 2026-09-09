// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"sync"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// ProjectReader is an in-memory port.ProjectReader double. Settings and
// forming-project entries are seeded directly by tests; there is no NATS
// round trip.
type ProjectReader struct {
	mu       sync.Mutex
	settings map[string]*port.ProjectSettings
	forming  []port.ProjectRef
	// byUID answers the uids half of the list, whatever stage the project is
	// at. Seeded separately from forming precisely so a test can put a project
	// at Active or Archived — a stage the forming list must not contain — and
	// still have it come back when the sweep names it.
	byUID map[string]port.ProjectRef
}

// NewProjectReader constructs an empty double.
func NewProjectReader() *ProjectReader {
	return &ProjectReader{
		settings: make(map[string]*port.ProjectSettings),
		byUID:    make(map[string]port.ProjectRef),
	}
}

// SetSettings seeds the settings returned for a project.
func (r *ProjectReader) SetSettings(projectUID string, s *port.ProjectSettings) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settings[projectUID] = s
}

// SetFormingProjects seeds the list ListFormingProjects returns.
func (r *ProjectReader) SetFormingProjects(refs []port.ProjectRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forming = refs
}

// GetSettings returns the seeded settings for a project, or domain.ErrNotFound.
func (r *ProjectReader) GetSettings(_ context.Context, projectUID string) (*port.ProjectSettings, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.settings[projectUID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	out := *s
	return &out, nil
}

// SetProjectsByUID seeds the projects the uids half of the list can answer with,
// at whatever stage the test gives them.
//
// Replaces the set rather than adding to it, matching SetFormingProjects — the
// two slice-shaped seeders on this double should not mean different things.
func (r *ProjectReader) SetProjectsByUID(refs []port.ProjectRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byUID = make(map[string]port.ProjectRef, len(refs))
	for _, ref := range refs {
		r.byUID[ref.UID] = ref
	}
}

// ListFormingProjects returns the seeded forming projects unioned with the named
// UIDs, deduplicated by UID — the same shape the subject upstream answers with.
//
// The union is reproduced here rather than simplified to the forming list,
// because the whole reason the caller passes UIDs is to see projects that have
// left formation. A double that ignored them would let a test assert a
// completed or frozen lifecycle that the deployed sweep can never reach.
func (r *ProjectReader) ListFormingProjects(_ context.Context, alsoUIDs []string) ([]port.ProjectRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]port.ProjectRef, 0, len(r.forming)+len(alsoUIDs))
	seen := make(map[string]bool, len(r.forming)+len(alsoUIDs))
	for _, ref := range r.forming {
		out = append(out, ref)
		seen[ref.UID] = true
	}
	for _, uid := range alsoUIDs {
		if seen[uid] {
			continue
		}
		// A named UID with nothing seeded for it is skipped rather than
		// answered with a blank ref, matching the upstream handler: a UID naming
		// no project it can read is not an error, it is simply absent.
		if ref, ok := r.byUID[uid]; ok {
			out = append(out, ref)
			seen[uid] = true
		}
	}
	return out, nil
}
