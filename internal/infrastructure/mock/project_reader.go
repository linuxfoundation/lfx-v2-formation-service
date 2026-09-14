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
	names    map[string]string
	forming  []port.ProjectRef
	// byUID answers the uids half of the list, whatever stage the project is
	// at. Seeded separately from forming precisely so a test can put a project
	// at Active or Archived — a stage the forming list must not contain — and
	// still have it come back when the sweep names it.
	byUID map[string]port.ProjectRef

	// refErr forces GetRef to fail, for the case a write-path refresh has to
	// survive: the owning service being unreachable. Distinct from a project
	// that is simply absent, which GetRef answers with ErrNotFound from the
	// maps above — the two mean different things to a caller counting a failed
	// refresh against a project it has nothing to publish for.
	refErr error

	nameCalls     int
	getRefCalls   int
	settingsCalls int
}

// NewProjectReader constructs an empty double.
func NewProjectReader() *ProjectReader {
	return &ProjectReader{
		settings: make(map[string]*port.ProjectSettings),
		names:    make(map[string]string),
		byUID:    make(map[string]port.ProjectRef),
	}
}

// SetName seeds the display name returned for a project.
func (r *ProjectReader) SetName(projectUID, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names[projectUID] = name
}

// Name returns the seeded display name, or domain.ErrNotFound.
//
// Counts its calls, so a test can assert the projection does not issue one
// lookup per row once the name arrives on the list reply.
func (r *ProjectReader) Name(_ context.Context, projectUID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nameCalls++
	name, ok := r.names[projectUID]
	if !ok {
		return "", domain.ErrNotFound
	}
	return name, nil
}

// NameCalls reports how many times Name was asked.
func (r *ProjectReader) NameCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nameCalls
}

// SettingsCalls reports how many times GetSettings was asked. Counted because
// it is a project-service round trip like Name and GetRef, and a cost ledger
// that omits it understates what a refresh actually spends.
func (r *ProjectReader) SettingsCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settingsCalls
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

	r.settingsCalls++
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

// SetRefError forces GetRef to fail with err. Passing nil clears it.
func (r *ProjectReader) SetRefError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refErr = err
}

// GetRef returns one project's ref from either seeded set, or
// domain.ErrNotFound.
//
// It answers from the forming list as well as the by-UID map, because a test
// that seeded only SetFormingProjects has still said the project exists. Making
// it read one map would mean every test wanting a refresh had to seed the same
// project twice, and the one that forgot would see a not-found that says
// nothing about the code under test.
func (r *ProjectReader) GetRef(_ context.Context, projectUID string) (port.ProjectRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.getRefCalls++
	if r.refErr != nil {
		return port.ProjectRef{}, r.refErr
	}
	if ref, ok := r.byUID[projectUID]; ok {
		return ref, nil
	}
	for _, ref := range r.forming {
		if ref.UID == projectUID {
			return ref, nil
		}
	}
	return port.ProjectRef{}, domain.ErrNotFound
}

// GetRefCalls reports how many times GetRef was asked, so a test can assert a
// write triggered exactly one refresh rather than none or several.
func (r *ProjectReader) GetRefCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getRefCalls
}
