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

**Indexed on:** every reconcile sweep tick, alongside the checklist (`formation`) document for the
same project — not on the item's own write. See [Cadence](#cadence).

### Data Schema

These fields are indexed and queryable via `filters`, `cel_filter`, or the tags below in the query
service.

| Field | Type | Description |
|---|---|---|
| `object_id` | string (UUID) | The item's own UID — this document's primary key, distinct from `formation_uid` |
| `formation_uid` | string (UUID) | UID of the checklist this item belongs to |
| `project_uid` | string | UID of the owning project, resolved through the formation rather than stored on the item |
| `item_key` | string | Stable across template versions; not the document's identity, useful for debugging a specific template row across formations |
| `title` | string | Item title |
| `status` | string enum | `not_started \| in_progress \| blocked \| awaiting_acceptance \| done \| skipped` |
| `gate` | bool | Whether this item blocks the project's Active transition |
| `due_date` | string (ISO date, optional) | Omitted when unset |
| `owner_team` | string (optional) | Omitted when unset |
| `action_link` | string (optional) | Omitted when unset. Says whether a link exists; says nothing about who may use it — the per-caller capability to act is a live check on the action itself, outside this document's scope |
| `sub_items` | []object | Nested, display-only sub-item summary, each with `key`, `title`, `status`. Nothing about it is separately filterable |
| `assignee` | string (optional) | Omitted, not empty-stringed, when the item is unassigned |

**Deliberately excluded** (drawer-only detail, read from the checklist directly when a caller opens
an item, never from this document): `note`, `skip_reason`, `resolved_ref`, `evidence_link`.

### Tags

| Tag Format | Example | Purpose |
|---|---|---|
| `project_uid:{value}` | `project_uid:cbef1ed5-17dc-4a50-84e2-6cddd70f6878` | Find items by project, for parity with the checklist document's own tag set |
| `formation_uid:{value}` | `formation_uid:01JQ0000000000000000000000` | Find items by checklist, for the same reason |
| `assignee:{value}` | `assignee:jdoe` | Find items assigned to a caller across every formation project they can access — the one tag Pending Actions depends on |

> The `assignee:` tag is only emitted when the item has an assignee.

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

### Cadence

Published from `Projector.Refresh`, on the same reconcile tick that already republishes the
checklist document — not synchronously from the item-write path. An item write (assignment, status
change) reaches the index within one reconcile interval, the same freshness the checklist's own
`assignee:` tags already carry for the same kind of edit today. A failed publish is repaired by the
next sweep tick; there is no separate backfill step for items that predate this feature, because the
sweep already visits every formation project on every tick regardless of when this feature shipped.

### Deletion

`DeleteItem` mirrors the checklist document's own `DeleteFormation` exactly — bare item UID as
`data`, no `indexing_config`. It is an operator-run escape hatch with no automatic caller today: no
checklist-deletion operation exists in this service (`FormationRepository` has no `Delete` method),
so `DeleteFormation` itself has no production caller either. An item's document is not removed
automatically when its checklist disappears; this is a pre-existing gap the checklist document
already has, inherited unchanged rather than newly introduced by this document.
