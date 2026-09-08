// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
)

// ProjectClient reads project facts over request/reply to the service that owns
// them. Nothing read here is stored: a copy of another service's data is a
// second source of truth that drifts.
//
// # What this can and cannot answer
//
// The project service exposes per-attribute lookups — name, slug, parent UID,
// logo, writers — and nothing that returns the settings record whole. Verified
// against its own subscription list rather than assumed.
//
// Two things this service needs are therefore not reachable yet:
//
//   - The auditors list. Only writers have a subject. The settings record does
//     carry auditors, but no subject returns it, so assignment validation
//     cannot ask "writer or auditor?" without an upstream addition. Filling
//     writers alone would be worse than not answering: it would refuse every
//     legitimate auditor with assignee_not_on_project, turning a check that is
//     currently inert into one that is actively wrong.
//   - The announcement date, for the same reason and from the same record.
//
// So this type deliberately does not implement port.ProjectReader. It provides
// the reads that exist, and the port stays unwired until the settings read
// does — which keeps the gap visible in the wiring instead of buried in a
// struct with two permanently empty fields.
type ProjectClient struct {
	client *Client
}

// NewProjectClient wires a project client over the shared NATS client.
func NewProjectClient(client *Client) *ProjectClient {
	return &ProjectClient{client: client}
}

// Name resolves the project's display name.
func (p *ProjectClient) Name(ctx context.Context, projectUID string) (string, error) {
	return p.get(ctx, ProjectGetNameSubject, projectUID)
}

// Slug resolves the project's slug.
func (p *ProjectClient) Slug(ctx context.Context, projectUID string) (string, error) {
	return p.get(ctx, ProjectGetSlugSubject, projectUID)
}

// Writers returns the usernames holding the project's write grant, which is the
// half of the direct-grant roster the project service exposes.
//
// A project with no writers configured is a real state and not an error: it
// replies with an empty JSON array. That is distinct from a zero-byte reply,
// which is how the project service reports every handler failure — an unknown
// project, an unparseable UID and a store outage all arrive that way, so it is
// read as not-found rather than decoded.
func (p *ProjectClient) Writers(ctx context.Context, projectUID string) ([]string, error) {
	if projectUID == "" {
		return nil, fmt.Errorf("project_uid is required: %w", domain.ErrInvalidRequest)
	}

	reply, err := p.client.Request(ctx, ProjectGetWritersSubject, []byte(projectUID))
	if err != nil {
		return nil, err
	}

	// Checked before unmarshalling: an absent reply would otherwise surface as
	// a JSON syntax error carrying no sentinel, leaving a caller unable to tell
	// "no such project" from "upstream is broken" — and an empty roster is what
	// assignee validation refuses against, so the two must not be confused.
	if len(bytes.TrimSpace(reply)) == 0 {
		return nil, fmt.Errorf("project %s returned no writers reply: %w", projectUID, domain.ErrNotFound)
	}

	// The reply carries name, email, username and avatar per entry. Only the
	// username is taken: it is the identifier an assignee is recorded as, and
	// keeping the rest would be storing personal data this service has no use
	// for.
	var writers []struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(reply, &writers); err != nil {
		return nil, fmt.Errorf("decoding writers for %s: %w", projectUID, err)
	}

	usernames := make([]string, 0, len(writers))
	for _, w := range writers {
		if w.Username != "" {
			usernames = append(usernames, w.Username)
		}
	}
	return usernames, nil
}

// get performs a single-attribute lookup, where the reply is the value as raw
// bytes and an empty reply means the project has no such attribute.
func (p *ProjectClient) get(ctx context.Context, subject, projectUID string) (string, error) {
	if projectUID == "" {
		return "", fmt.Errorf("project_uid is required: %w", domain.ErrInvalidRequest)
	}

	reply, err := p.client.Request(ctx, subject, []byte(projectUID))
	if err != nil {
		return "", err
	}

	value := strings.TrimSpace(string(reply))
	if value == "" {
		return "", fmt.Errorf("project %s has no value on %s: %w", projectUID, subject, domain.ErrNotFound)
	}
	return value, nil
}
