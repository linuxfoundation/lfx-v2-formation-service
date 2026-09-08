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
}

// NewProjectReader constructs an empty double.
func NewProjectReader() *ProjectReader {
	return &ProjectReader{settings: make(map[string]*port.ProjectSettings)}
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

// ListFormingProjects returns the seeded forming-project list.
func (r *ProjectReader) ListFormingProjects(_ context.Context) ([]port.ProjectRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]port.ProjectRef, len(r.forming))
	copy(out, r.forming)
	return out, nil
}
