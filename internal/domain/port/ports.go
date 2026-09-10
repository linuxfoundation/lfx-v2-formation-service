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

	// ListFormingProjects returns the projects in a formation sub-stage, plus
	// any project named in alsoUIDs whatever stage it is at now.
	//
	// The second half is what makes the sweep able to finish what it started.
	// A checklist is completed when its project goes Active and frozen when it
	// is archived, and both of those stages are, by definition, not formation
	// stages — so a forming-only list drops exactly the projects whose
	// lifecycle still needs moving, and those two transitions could never run
	// in a deployed pod. The caller passes the projects it holds a checklist
	// for, and gets their current stage back whatever it is.
	ListFormingProjects(ctx context.Context, alsoUIDs []string) ([]ProjectRef, error)

	// Name resolves the project's display name, one project per call.
	//
	// TODO: fold this into the list reply. The queue needs a name for every row
	// it renders, and the list reply carries uid, slug, is_foundation,
	// parent_uid and stage — everything but the one field the page displays. So
	// a queue of N rows costs N of these lookups on top of the one list request,
	// which is the per-row round trip that unioning the list filters existed to
	// avoid in the first place. Adding name to the list reply is a
	// project-service change, so it is a separate PR there; this is here to
	// unblock the projection without waiting on it.
	//
	// The fan-out is deliberately left serial in the meantime. This is resolved
	// at publish time on the reconcile ticker, never on a read path — the queue
	// screen is one search against the index and never reaches this service — so
	// the cost is sweep duration over the projects being formed, not latency on
	// anybody's page. Folding the name into the list reply removes the fan-out
	// outright, which is why it is that rather than concurrency here.
	Name(ctx context.Context, projectUID string) (string, error)
}

// Subscriber delivers messages published by other services to a handler.
//
// The counterpart to ProjectReader: that one asks and waits, this one is told.
// Both exist because the two answer different questions — the read is how the
// sweep finds out what is true now, and the subscription is how the service
// finds out sooner that something changed.
//
// Nothing here is required for correctness, and the interface is deliberately
// too thin to promise otherwise. There is no acknowledgement, no redelivery and
// no replay, so a handler that fails has no way to ask for the message again and
// a service that is not running receives nothing. Whatever consumes this must be
// an accelerator behind something that establishes the same outcome on its own.
type Subscriber interface {
	// Subscribe delivers messages on subject to handler, sharing the work with
	// the other members of queue so that exactly one of them handles each
	// message.
	//
	// The queue name scopes the sharing to this service. Other services
	// consuming the same subject use their own name and receive their own copy,
	// which is what lets one replica per service handle an event while every
	// interested service still sees it.
	//
	// The returned function stops delivery and waits for the handler to finish.
	// Waiting is the point: a handler is inside a database transaction, and
	// tearing it down mid-flight during shutdown would abort work that had
	// already been decided on.
	Subscribe(ctx context.Context, subject, queue string, handler func(data []byte)) (stop func(), err error)
}

// IndexerPublisher publishes a checklist's search projection.
//
// The queue screen is one access-filtered search against this projection and
// nothing else — no per-row request, no fan-out — so what is not published here
// cannot be shown there. That is the whole reason the interface exists at this
// layer: the projection is a product decision about what staff may see, and it
// should be reviewable without reading a NATS client.
//
// Publishing is best-effort by design. Postgres is the source of truth, the
// projection is derived, and the reconcile republishes on every sweep, so a
// failed publish is repaired by the next one. A caller must therefore never
// fail a user's write because this failed.
//
// How long that repair takes is a deployment decision rather than a property of
// this interface, and it is getting longer: the sweep is moving to a daily
// cadence, so "the next sweep fixes it" means within a day rather than within
// fifteen minutes. Best-effort is still the right trade for a derived document,
// but the window is now wide enough that an operator-run repair exists
// alongside it rather than as a theoretical escape hatch.
type IndexerPublisher interface {
	// PublishFormation upserts one checklist's projection.
	PublishFormation(ctx context.Context, doc *FormationProjection) error

	// DeleteFormation removes one checklist's projection from the index.
	//
	// Keyed on the formation UID because that is the document's identity — the
	// same value PublishFormation sends as the object ID.
	//
	// Separate from PublishFormation rather than an action argument on it. The
	// upsert has no legitimate reason to ever delete, and a shared entry point
	// would put "which action?" on a path that publishes on every sweep for
	// every project.
	//
	// There is deliberately no caller in the sweep. A checklist is never
	// deleted, so the only way its row is orphaned is the project behind it
	// disappearing — which the sweep cannot observe, because it lists forming
	// projects and a project that is gone is not in any list. Removal is
	// therefore an operator-run repair, and this exists for that job to call.
	DeleteFormation(ctx context.Context, formationUID string) error
}

// FormationProjection is one row of the Formations queue.
//
// Everything here is either owned by this service or read fresh from the
// project service on the sweep that publishes it. None of the project-owned
// fields are stored in this service's database: a copy that outlives its source
// is a second truth that drifts, and the sweep that refreshes this is the same
// one that would have had to invalidate a cache.
type FormationProjection struct {
	// FormationUID identifies the document. The project UID would have done
	// equally well, since the relationship is one-to-one, but keying on the
	// checklist keeps the document's identity owned by this service.
	FormationUID string
	ProjectUID   string

	// Project facts, read fresh on each sweep and never stored here.
	ProjectName      string
	ProjectSlug      string
	IsFoundation     bool
	ParentUID        string
	SubStage         string
	AnnouncementDate string

	// Lifecycle is carried so the queue can tell a live checklist from one that
	// completed or froze, rather than inferring it from the stage.
	Lifecycle string

	// GatesCleared is the gates-only half of readiness: every gating item done,
	// and at least one gating item exists. It deliberately excludes the
	// announcement date, which travels beside it — see the projection builder
	// for why the two halves are published separately.
	GatesCleared bool

	// IsActivating is full readiness: GatesCleared and an announcement date set.
	IsActivating bool

	// The six progress counts, sent as fields so the search can sort on them.
	NotStarted         int
	InProgress         int
	Blocked            int
	AwaitingAcceptance int
	Done               int
	Skipped            int

	// BlockedItemTitles is the Blocking column. Titles only — naming the person
	// on a blocked item would put an assignment in a document read by everyone
	// holding the project's audit relation.
	BlockedItemTitles []string

	// Assignees is what the "Mine" filter matches on.
	Assignees []string

	// AccessRelation is the relation a caller must hold on the project to read
	// this row.
	//
	// Carried on the document rather than chosen by the publisher, so the
	// decision sits in the domain layer beside the fields it protects and is
	// reviewable without reading a NATS client. The publisher refuses a document
	// that does not set it: a missing relation is not a default to fill in.
	AccessRelation string
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
