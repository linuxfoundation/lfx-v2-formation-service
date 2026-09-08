// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package port declares the interfaces the domain depends on. Storage and
// messaging choices stay behind these, so the service layer never imports a
// driver.
package port

import (
	"context"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// FormationRepository stores formations. Create is idempotent by way of the
// uniqueness constraint on project_uid rather than a read-then-write check, so
// concurrent replicas cannot both succeed.
type FormationRepository interface {
	// Create inserts a formation. It returns ErrAlreadyExists when one
	// already exists for the project, which callers treat as success.
	Create(ctx context.Context, f *model.Formation) (*model.Formation, error)

	// GetByProject returns the formation for a project, or ErrNotFound.
	GetByProject(ctx context.Context, projectUID string) (*model.Formation, error)

	// UpdateLifecycle moves the formation's lifecycle, refusing the write
	// when revision does not match the caller's copy.
	UpdateLifecycle(ctx context.Context, uid uuid.UUID, lifecycle model.Lifecycle, revision int64) (*model.Formation, error)

	// UpdateSections replaces the section snapshot, refusing the write when
	// revision does not match the caller's copy. Used only by the upgrade job,
	// to record a section a newer template introduced that this checklist has
	// not seen. A version mismatch is not fatal to the caller: the write is
	// idempotent, so a run that loses this race leaves the gap for the next
	// run to close rather than losing the items it already added.
	UpdateSections(ctx context.Context, uid uuid.UUID, sections []model.FormationSection, revision int64) (*model.Formation, error)

	// ListProjectUIDs returns the projects that already have a formation,
	// so the reconcile loop can find the ones that do not.
	ListProjectUIDs(ctx context.Context) ([]string, error)
}

// ItemRepository stores checklist items. Every mutation carries the caller's
// revision and fails rather than overwriting a newer value.
type ItemRepository interface {
	// InsertMany expands a checklist. Items already present by key are left
	// untouched, which is what makes expansion and upgrade idempotent.
	//
	// It returns the keys it actually inserted, which is not always every key
	// passed in: a concurrent caller may have inserted some of them first. A
	// caller recording what it added has to use this rather than its own input,
	// or two racing upgrades both claim to have added the same items.
	InsertMany(ctx context.Context, items []*model.Item) ([]string, error)

	// ListByFormation returns every item, ordered by section then position.
	ListByFormation(ctx context.Context, formationUID uuid.UUID) ([]*model.Item, error)

	Get(ctx context.Context, uid uuid.UUID) (*model.Item, error)

	// GetByKey returns the item at item_key within a formation, or
	// ErrNotFound. Mutation routes address an item by its stable key, not
	// its UID, so this is the lookup they actually need.
	GetByKey(ctx context.Context, formationUID uuid.UUID, itemKey string) (*model.Item, error)

	// Update applies the mutable fields carried by patch. It returns
	// ErrVersionMismatch when the caller's revision is stale, which the
	// transport reports as a failed precondition rather than a conflict.
	Update(ctx context.Context, uid uuid.UUID, revision int64, patch ItemPatch) (*model.Item, error)
}

// ItemPatch carries the mutable fields of an item. A nil pointer means leave
// the field alone, which is what distinguishes "not supplied" from "cleared".
type ItemPatch struct {
	Status       *model.ItemStatus
	Assignee     *string
	Note         *string
	SkipReason   *string
	EvidenceLink *string
	DueDate      *string
	ResolvedRef  *model.ResolvedRef
	SubItems     *[]model.SubItem
}

// ActivityRepository appends to the feed and reads it back. There is
// deliberately no update or delete: the feed is immutable.
type ActivityRepository interface {
	Append(ctx context.Context, e *model.ActivityEntry) error

	// List returns entries newest first. cursor is the ULID of the last
	// entry from the previous page, or empty for the first page.
	List(ctx context.Context, formationUID uuid.UUID, cursor string, limit int) ([]*model.ActivityEntry, string, error)
}

// TemplateRepository reads and seeds templates.
type TemplateRepository interface {
	// ListPublished returns published templates ordered by priority and, within
	// one priority, newest version first, so selection is a first-match walk
	// over the result. Publishing a version does not retire the one before it,
	// so the version ordering is what keeps the choice between them from
	// depending on row order.
	ListPublished(ctx context.Context) ([]*model.Template, error)

	Get(ctx context.Context, uid uuid.UUID) (*model.Template, error)

	// Upsert seeds a template version, or re-seeds one that is not yet
	// published. Keyed on name and version, so re-running the seed job is a
	// no-op. Changing the content of an already-published version is refused
	// with domain.ErrConflict: live checklists pin the version they expanded
	// from, so editing it in place would change what those pins mean.
	Upsert(ctx context.Context, t *model.Template) (*model.Template, error)
}

// UnitOfWork runs a function against repositories bound to one transaction, so
// an item change and its activity entry commit together or not at all.
type UnitOfWork interface {
	Do(ctx context.Context, fn func(Tx) error) error
}

// Tx exposes the repositories that share one transaction.
type Tx interface {
	Formations() FormationRepository
	Items() ItemRepository
	Activity() ActivityRepository
	Templates() TemplateRepository
}

// ProjectReader reads the project facts this service needs and never stores.
// Backed by NATS request/reply to the owning service, so no token travels on
// the wire.
type ProjectReader interface {
	// GetSettings returns the announcement date, writers and auditors from
	// the project's settings.
	GetSettings(ctx context.Context, projectUID string) (*ProjectSettings, error)

	// ListFormingProjects returns the projects in a formation sub-stage,
	// which is what the reconcile loop sweeps.
	ListFormingProjects(ctx context.Context) ([]ProjectRef, error)
}

// ProjectSettings is the subset of a project's settings this service reads.
type ProjectSettings struct {
	ProjectUID       string
	AnnouncementDate *string
	Writers          []string
	Auditors         []string
}

// ProjectRef identifies a project and the facts template selection needs.
type ProjectRef struct {
	UID          string
	Slug         string
	IsFoundation bool
	ParentUID    string
	SubStage     string
}

// ResourceChecker answers a platform check by asking the service that owns the
// resource, over NATS request/reply.
type ResourceChecker interface {
	// Count reports how many resources of the given type the project has,
	// and a reference to one of them when the check resolves.
	Count(ctx context.Context, projectUID, resourceType string) (int, *model.ResolvedRef, error)
}

// Publisher emits messages for the indexer and for other services.
type Publisher interface {
	Publish(ctx context.Context, subject string, payload []byte) error
}
