// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// ProjectClient reads project facts over request/reply to the service that owns
// them. Nothing read here is stored: a copy of another service's data is a
// second source of truth that drifts.
//
// # What this answers
//
// Per-attribute lookups for name and slug, the writers list, and — since the
// project service added them — the settings record as a whole and a project
// list filtered by stage. The last two are what this type needed to satisfy
// port.ProjectReader at all: before they existed the auditors list and the
// announcement date had no subject, and nothing could enumerate the projects
// being formed, so the port was left unwired rather than implemented with two
// permanently empty fields.
//
// Nothing here is stored. Every reply is used and discarded, so a stage change
// upstream is visible on the next read rather than after an invalidation this
// service would have to get right.
type ProjectClient struct {
	client *Client
}

// Compile-time check that this satisfies the port. Worth stating explicitly:
// the provider returns the interface, so a signature drifting out of line
// would otherwise surface as a nil reader at runtime — which reads exactly like
// the not-yet-wired state this type spent its first version in.
var _ port.ProjectReader = (*ProjectClient)(nil)

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
	var writers []projectUser
	if err := json.Unmarshal(reply, &writers); err != nil {
		return nil, fmt.Errorf("decoding writers for %s: %w", projectUID, err)
	}
	return usernames(writers), nil
}

// projectUser is the per-person shape inside a grant roster. Username is the
// identifier an assignee is recorded as. Email is carried alongside it so
// notification dispatch can route to a real mailbox: the service stores
// usernames, not addresses, so email must be resolved at send time from the
// same grant roster that accepted the assignee.
type projectUser struct {
	Username string `json:"username"`
	Email    string `json:"email"`
}

// projectSettingsReply is the get_settings reply, declared here rather than
// imported from the project service.
//
// Not a rule against cross-service imports — this service depends on the
// indexer's pkg/types, as six others do, because the indexer publishes its
// envelope as a supported contract under pkg/. The project service publishes
// these lookup shapes under pkg/events too, so importing them would be
// defensible. Restating three fields is the smaller commitment: it is the
// reply's *shape* that has to hold, not its Go type, and the alternative ties
// this service's build to that repository's release cadence for a struct that
// would not shrink if imported.
type projectSettingsReply struct {
	UID              string        `json:"uid"`
	AnnouncementDate *time.Time    `json:"announcement_date"`
	Writers          []projectUser `json:"writers"`
	Auditors         []projectUser `json:"auditors"`
}

// projectListRequest asks for projects by stage, by UID, or both. The reply is
// their union.
type projectListRequest struct {
	Stages []string `json:"stages,omitempty"`
	UIDs   []string `json:"uids,omitempty"`
}

// projectRefReply is one entry in the list_projects reply. Stage is the compound
// value — "Formation - Engaged" and the like — because there is no separate
// sub-stage field upstream.
type projectRefReply struct {
	UID          string `json:"uid"`
	Slug         string `json:"slug"`
	IsFoundation bool   `json:"is_foundation"`
	ParentUID    string `json:"parent_uid"`
	Stage        string `json:"stage"`
}

// GetSettings returns the announcement date and both halves of the grant roster
// from the project's settings record.
//
// A project with no settings record is reported as not found rather than as an
// empty roster. The difference decides an assignment: an empty roster is what
// assignee validation refuses against, so reading "no record" as "nobody is on
// this project" would turn an upstream gap into a refusal of every legitimate
// assignee.
func (p *ProjectClient) GetSettings(ctx context.Context, projectUID string) (*port.ProjectSettings, error) {
	if projectUID == "" {
		return nil, fmt.Errorf("project_uid is required: %w", domain.ErrInvalidRequest)
	}

	reply, err := p.client.Request(ctx, ProjectGetSettingsSubject, []byte(projectUID))
	if err != nil {
		return nil, err
	}
	// Checked before unmarshalling, as the writers read does: a zero-byte reply
	// is how the project service reports every handler failure, so it carries no
	// sentinel of its own and would otherwise surface as a JSON syntax error.
	if len(bytes.TrimSpace(reply)) == 0 {
		return nil, fmt.Errorf("project %s returned no settings reply: %w", projectUID, domain.ErrNotFound)
	}

	var decoded projectSettingsReply
	if err := json.Unmarshal(reply, &decoded); err != nil {
		return nil, fmt.Errorf("decoding settings for %s: %w", projectUID, err)
	}

	settings := &port.ProjectSettings{
		ProjectUID: projectUID,
		Writers:    usernames(decoded.Writers),
		Auditors:   usernames(decoded.Auditors),
	}
	// Build a username→email map from the combined roster so callers that
	// need to send email to a named grantee can resolve their address without
	// a second NATS round-trip. Writers and Auditors share one map: both
	// halves appear in the People panel and either may be an assignee or
	// notification recipient. When the same username appears in both, the
	// writer entry wins (first write), but in practice duplicates carry the
	// same address.
	allUsers := make([]projectUser, 0, len(decoded.Writers)+len(decoded.Auditors))
	allUsers = append(allUsers, decoded.Writers...)
	allUsers = append(allUsers, decoded.Auditors...)
	settings.UserEmails = userEmailMap(allUsers)
	// Narrowed to a date deliberately. Due dates are computed by offsetting
	// whole days from this, so the time of day is precision the calculation
	// cannot use and would only introduce timezone questions into a comparison
	// that has none.
	if decoded.AnnouncementDate != nil && !decoded.AnnouncementDate.IsZero() {
		formatted := decoded.AnnouncementDate.UTC().Format(time.DateOnly)
		settings.AnnouncementDate = &formatted
	}
	return settings, nil
}

// ListFormingProjects returns the projects at a formation stage, together with
// any project named in alsoUIDs whatever stage it has reached.
//
// One request, not two: the subject unions the filters, and the caller needs
// both halves for a single decision, so splitting them would let the sweep act
// on a set assembled from two different moments.
//
// An empty alsoUIDs asks for the stages alone, which is the ordinary case once
// no checklist has outlived its project's formation.
func (p *ProjectClient) ListFormingProjects(ctx context.Context, alsoUIDs []string) ([]port.ProjectRef, error) {
	request, err := json.Marshal(projectListRequest{
		Stages: model.FormationStages(),
		UIDs:   alsoUIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding the project list request: %w", err)
	}

	reply, err := p.client.Request(ctx, ProjectListProjectsSubject, request)
	if err != nil {
		return nil, err
	}
	// A zero-byte reply is an upstream failure, and here the distinction from an
	// empty list matters more than anywhere else: read as "no projects are being
	// formed" it would look like a completed sweep with nothing to do, and the
	// reconcile would report success having created nothing.
	if len(bytes.TrimSpace(reply)) == 0 {
		return nil, fmt.Errorf("project list returned no reply: %w", domain.ErrNotFound)
	}

	var decoded []projectRefReply
	if err := json.Unmarshal(reply, &decoded); err != nil {
		return nil, fmt.Errorf("decoding the project list: %w", err)
	}

	refs := make([]port.ProjectRef, 0, len(decoded))
	for _, entry := range decoded {
		if entry.UID == "" {
			// Nothing can be done with a ref that names no project: it cannot
			// be swept, and passing it on would have the sweep count it.
			continue
		}
		refs = append(refs, port.ProjectRef{
			UID:          entry.UID,
			Slug:         entry.Slug,
			IsFoundation: entry.IsFoundation,
			ParentUID:    entry.ParentUID,
			// The port calls this SubStage and the wire calls it stage; they are
			// the same compound value. The name is the older one, from before it
			// was established that no separate sub-stage field exists.
			SubStage: entry.Stage,
		})
	}
	return refs, nil
}

// usernames reduces a grant roster to the identifiers an assignee is recorded
// as, dropping entries with no username rather than carrying an empty string
// that would match an unassigned item.
func usernames(users []projectUser) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u.Username != "" {
			out = append(out, u.Username)
		}
	}
	return out
}

// userEmailMap builds a username→email index from a grant roster, used by
// notification dispatch to route to a real mailbox when the stored assignee is
// a username. Entries with a blank username or blank email are skipped:
// a blank username has no key to index under, and a blank email would silently
// overwrite a good entry if the same username appeared twice.
func userEmailMap(users []projectUser) map[string]string {
	out := make(map[string]string, len(users))
	for _, u := range users {
		if u.Username != "" && u.Email != "" {
			if _, exists := out[u.Username]; !exists {
				out[u.Username] = u.Email
			}
		}
	}
	return out
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
