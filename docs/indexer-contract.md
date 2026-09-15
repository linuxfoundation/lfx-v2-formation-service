# Indexer Contract — Formation Service

This document is the authoritative reference for data the formation service sends to the indexer
service, which makes resources searchable via the [query service](https://github.com/linuxfoundation/lfx-v2-query-service).

**Update this document in the same PR as any change to indexer message construction.**

**Scope note:** this service publishes two object types — `formation` (the checklist queue row)
and `formation_item` (this document's subject). Only `formation_item` is documented here; it is the
one this feature introduces. `formation`'s own contract is not yet written down — see
`internal/infrastructure/nats/indexer_publisher.go`'s `PublishFormation`/`projectionData` for its
current shape until a follow-up documents it here too.

---

## Resource Types

- [Formation Item](#formation-item)

---

## Formation Item

**Object type:** `formation_item`

**NATS subject:** `lfx.index.formation_item`

**Source struct:** `internal/domain/model/formation.go` — `Item`, via `internal/domain/port/ports.go`
— `ItemProjection`

**Indexed on:** every item write, and on every reconcile sweep tick as a backstop — always alongside
the checklist (`formation`) document for the same project. See [Cadence](#cadence).

### Data Schema

These fields are indexed and queryable via `filters`, `cel_filter`, or the tags below in the query
service.

| Field | Type | Description |
|---|---|---|
| `object_id` | string (UUID) | The item's own UID — this document's primary key, distinct from `formation_uid` |
| `formation_uid` | string (UUID) | UID of the checklist this item belongs to |
| `project_uid` | string | UID of the owning project, resolved through the formation rather than stored on the item |
| `project_name` | string | Owning project's display name, so a row can name its project without a second read |
| `project_slug` | string | Owning project's slug, same reason |
| `lifecycle` | string enum | `live \| completed \| frozen` — the owning checklist's. A non-live checklist refuses mutations, so its unfinished items are history rather than open work; exclude them with the tag |
| `item_key` | string | Stable across template versions; not the document's identity, useful for debugging a specific template row across formations |
| `title` | string | Item title |
| `status_source` | string enum | `manual \| platform` — whether the status is hand-set or driven by a platform check. Drives the row's action affordance |
| `status` | string enum | `not_started \| in_progress \| blocked \| awaiting_acceptance \| done \| skipped` |
| `gate` | bool | Whether this item blocks the project's Active transition |
| `requires_writer` | bool | Whether acting on this item needs `writer` on the project. A fact about the item; whether the caller holds `writer` is a fact about the caller, which a shared document cannot carry. A row decides actionability from the two together |
| `due_date` | string (ISO date, optional) | Omitted when unset |
| `owner_team` | string (optional) | Omitted when unset |
| `action_link` | string (optional) | Omitted when unset. Says whether a link exists; says nothing about who may use it — the per-caller capability to act is a live check on the action itself, outside this document's scope |
| `sub_items` | []object | Nested, display-only sub-item summary, each with `key`, `title`, `status`. Nothing about it is separately filterable |
| `assignee` | string (optional) | Omitted, not empty-stringed, when the item is unassigned |

**Deliberately excluded** (drawer-only detail, read from the checklist directly when a caller opens
an item, never from this document): `note`, `skip_reason`, `resolved_ref`, `evidence_link`.

Also excluded: `version`, the `If-Match` token the mutation endpoints require. A document this old
could only hand out a stale one. Read the item to act on it — and once a caller has written, the
mutation response returns the next token as `ETag`, so no re-read is needed to keep writing.

### Tags

| Tag Format | Example | Purpose |
|---|---|---|
| `project_uid:{value}` | `project_uid:cbef1ed5-17dc-4a50-84e2-6cddd70f6878` | Find items by project, for parity with the checklist document's own tag set |
| `formation_uid:{value}` | `formation_uid:01JQ0000000000000000000000` | Find items by checklist, for the same reason |
| `assignee:{value}` | `assignee:jdoe` | Find items assigned to a caller across every formation project they can access — the one tag Pending Actions depends on |
| `lifecycle:{value}` | `lifecycle:live` | Narrow to checklists that still accept changes |

> The `assignee:` tag is only emitted when the item has an assignee.

> These tags alone are not the query. Pending Actions sends:
>
> ```
> GET /query/resources?v=1&type=formation_item
>     &tags_all=assignee:<username>&tags_all=lifecycle:live
>     &cel_filter=data.status != "done" && data.status != "skipped"
> ```
>
> `type=formation_item` is required: the checklist document carries the same `assignee:` and
> `lifecycle:` tags, so without it every project adds a checklist document to a list of items.
> `tags_all` rather than `tags`, which is OR and would match either tag alone.

### Access Control (IndexingConfig)

| Field | Value |
|---|---|
| `access_check_object` | `project:{project_uid}` |
| `access_check_relation` | `auditor` |
| `history_check_object` | `project:{project_uid}` |
| `history_check_relation` | `auditor` |
| `public` | _(omitted; never public — an item is never visible to an anonymous caller)_ |

> **Access:** identical to the checklist document's own guard, applied at item grain rather than
> project grain — `auditor` on `project:{uid}`, never `viewer` (`viewer` on a project document
> includes `[user:*]`, and a formation item is not public). No new OpenFGA type, relation, or grant
> exists for this document.

### Search Behavior

| Field | Value |
|---|---|
| `fulltext` | _(none)_ |
| `name_and_aliases` | _(none)_ |
| `sort_name` | `title` |
| `public` | _(omitted; see Access Control)_ |

### Parent References

| Ref | Condition |
|---|---|
| `project:{project_uid}` | Always set |
| `formation:{formation_uid}` | Always set |

The item document carries its own project and checklist, and no ancestry beyond them. The checklist
document does carry the project's ancestor chain, so that a foundation's queue resolves at any
depth — but the two are deliberately different. This document's only consumer narrows by assignment
rather than by foundation, so a chain here would be surface with nothing reading it. Add one when a
consumer asks for it, not to make the two document types match.

That chain is what the service could resolve when it published, not a guarantee. The walk ends early
for two quite different reasons, and only one of them is worth holding a publish over.

An **ancestor that cannot be read** may well answer next time, so there is a better chain to wait
for. What happens then turns on whether a document for the row can already exist. A checklist
created in that same pass has none, so it publishes the prefix that resolved: present under fewer
foundations beats absent from all of them. Every other publish — a sweep revisiting a checklist, a
project event, an operator repair, a refresh after an item write — is replacing a document that may
already carry the full chain, so it is withheld and what is in the index stands until a later pass
resolves the whole chain.

A walk stopped by the **depth cap or a cycle** is as long as it will ever be: both are properties of
the data, so every later walk returns the identical prefix. These always publish, whatever the
posture. Withholding would not be waiting for anything — it would freeze the document entirely,
counts and stage included, for as long as the shape persisted, and no retry, sweep or repair command
could clear it.

Every short chain, withheld or published, increments `partial_chains_total` on the sweep's closing
log line. That counter is the only place the shortfall surfaces: a row scoped to less than its
parentage does not appear under the foundation it belongs to, and that is indistinguishable in a
result from the row not existing.

### Cadence

Published from `Projector.Refresh`, which two paths call:

- **On the write.** `PATCH /items/{key}`, `accept`, `reject` and `reopen` each ask for a refresh
  after their transaction commits. This is the path a person's list depends on: an item assigned to
  somebody appears on their queue in seconds rather than at the next sweep.
- **On the sweep.** Every reconcile tick republishes the same documents for every forming project,
  which is what repairs a write-path refresh lost to a restart, a timeout or an unreachable index.

Both produce identical documents — one projector, one document shape, so a reader cannot tell which
path published what it is reading. The one case where a publish is not identical does not affect
this document: when the checklist row is withheld for unresolved parentage (above), the item
documents still publish, alone. Nothing about an item document is uncertain when a project's
ancestors cannot be read, and holding them back would stall the assignee's list for a reason that
has nothing to do with them.

The write-path refresh is asynchronous and best-effort by design. It runs after the response has
been written, so it cannot fail or delay a write: the checklist in Postgres is the source of truth
and this index is derived from it. A failure is logged and counted, never returned, and the next
sweep repairs it. In-flight refreshes are drained at shutdown for up to
`DefaultRefreshDrainTimeout`, then cancelled — they hold database connections, so leaving them
running would only move the wait into the pool's close.

A refresh republishes the **whole project** — the checklist document and one document per item —
rather than the single item that was written. The checklist document carries counts over every
item, so republishing one item would leave the aggregate disagreeing with the rows it aggregates.

There is no separate backfill step for items that predate this feature, because the sweep already
visits every formation project on every tick regardless of when this feature shipped.

**Cost per write:** two database reads (the checklist row and its items), three NATS request/replies
to the project service (the project ref, its display name and its settings — only the ref is new,
the other two are what the projector has always read), one checklist publish and one batched item
publish covering every item at once. Scoped to a single project, so it does not grow with the number
of projects being formed. Pinned by `TestOneItemWriteRefreshCostsAKnownAmountOfIO`.

### Deletion

`DeleteItem` mirrors the checklist document's own `DeleteFormation` exactly — bare item UID as
`data`, no `indexing_config`. It is an operator-run escape hatch with no automatic caller today: no
checklist-deletion operation exists in this service (`FormationRepository` has no `Delete` method),
so `DeleteFormation` itself has no production caller either. An item's document is not removed
automatically when its checklist disappears; this is a pre-existing gap the checklist document
already has, inherited unchanged rather than newly introduced by this document.
