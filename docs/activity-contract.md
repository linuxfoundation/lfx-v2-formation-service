# Activity Feed Contract — Formation Service

This document is the authoritative reference for reading the checklist activity feed,
`GET /formations/{project_uid}/activity`.

**Update this document in the same PR as any change to that route's parameters, ordering, paging or
error reporting.**

Everything here is about how to *ask* for entries. Nothing about what the feed records — the entries,
their fields, when they are appended, or the transaction they commit in — belongs in this document or
is affected by it. The feed is append-only; the route is read-only.

The generated wire contract lives in `gen/` and is produced by `make apigen` from
`cmd/formation-api/design/design.go`. Where this document and the design disagree, the design is
authoritative and this document is stale.

---

## Request

```
GET /formations/{project_uid}/activity?v=1&item_uid={uuid}&cursor={ulid}&limit={1..100}
Authorization: Bearer <JWT forwarded by Heimdall>
```

| Parameter | In | Type | Required | Notes |
|---|---|---|---|---|
| `project_uid` | path | string | yes | The project whose checklist is being read |
| `v` | query | string | yes | Stays `1`; see [Versioning](#versioning) |
| `item_uid` | query | string, UUID | no | Narrows the feed to one item's history |
| `cursor` | query | string, ULID | no | From a previous page's `next_cursor`; see [Cursors](#which-sequence-a-cursor-belongs-to) |
| `limit` | query | int, 1–100 | no | Defaults to 20 |

Entries come back newest first, ordered by `ulid` descending. `next_cursor` is empty on the last page.

`action` has no wire enum. A consumer must tolerate an unrecognised value rather than coerce it.

---

## Why this route names an item by UID when its siblings name it by key

Every route that *mutates* an item addresses it by stable key — `PATCH /formations/{project_uid}/items/{item_key}`
and the `accept`, `reject` and `reopen` routes beside it. This route deliberately differs and takes the
item's **UID**.

The reason is in the row: an activity entry stores the item's UID as a foreign key
(`internal/infrastructure/postgres/schema.sql`, `formation_activity.item_uid`) and does not store the
key at all. A consumer filtering the feed already holds the UID, because every entry it is reading
carries one. Requiring the key would mean resolving an identity the row already has, on both sides.

This is written down rather than left to be inferred because inferring it wrong fails silently: a key
passed as `item_uid` is not a UUID, so it is refused at decode — but a *different* item's UID is not,
and returns that item's history instead of an error.

---

## Which sequence a cursor belongs to

A `next_cursor` is a position in **the sequence that produced it**, not in the feed generally.

- A cursor from a filtered read is only valid when replayed **with the same `item_uid`**.
- A cursor from an unfiltered read is only valid when replayed **without one**.

Mixing them is **not rejected and does not error**. It returns a correct page of a different sequence,
which is the dangerous outcome, because it looks like an answer. Callers are responsible for carrying
the filter alongside the cursor.

A filtered read pages by exactly the same mechanism as an unfiltered one — `limit`, then `next_cursor`
until it comes back empty — over the item's own history. An item with more history than one page holds
is fully reachable that way. Worth stating rather than implying: a filtered read usually returns fewer
entries than a page holds, so the multi-page path is the one that goes unexercised and then breaks.

---

## What an absent `item_uid` on an entry means

An entry with no `item_uid` is **either**:

- a formation-level entry — template expansion or upgrade, which concern no single item — **or**
- an entry whose item has since been removed, because `formation_activity.item_uid` is
  `ON DELETE SET NULL`: the reference is cleared rather than the row deleted.

The two are indistinguishable on the wire. The second is not currently reachable — nothing in this
service deletes a checklist item — but both meanings are stated so that a consumer does not read
"absent" as "formation-level" and render a detached entry as a template event.

Neither is ever returned by a filtered read, which matches on a supplied UUID.

---

## Errors

### 400 — a malformed parameter

An `item_uid` carrying a value that is not a well-formed UUID is refused at decode, by the same
mechanism that refuses `limit=-1`. It is not treated as "no filter", which would answer a different
question than the one asked, and it never reaches the database, where comparing a non-UUID against a
`UUID` column would surface as a server error.

**`?item_uid=` with an empty value is the exception, and is treated as absent** — it returns the whole
formation's feed rather than a 400. This is how the transport decodes every optional string parameter
(`cursor` behaves the same way): an absent parameter and a present-but-empty one are both the empty
string at decode, and the format check runs only on a non-empty value. It is not expressible in the
design, so it is stated here instead and pinned by a transport test.

The practical hazard is a consumer that templates the URL from an unset variable: it asks for one
item's history and is served the entire feed, believing it is scoped. There is no authorization
consequence — the response is exactly the unfiltered feed the caller is already entitled to — so this
is a correctness trap for the caller, not a leak. Send the parameter with a value or omit it entirely.

### 404 — two situations, told apart by message

| Situation | `message` |
|---|---|
| No formation exists for this project | `no formation exists for this project` |
| `item_uid` names no item in this project's formation | `no such item in this formation` |

`code` is `"404"` in both. The distinction is carried by `message` alone, because this route's
`NotFoundError` shape has only `code` and `message` — and adding a discriminator field would change
the error body for the formation case, which callers already depend on.

"Project" and "formation" name the same scope across those two messages. A project has exactly one
formation, so the item message's "formation" is not a second, narrower check.

The two messages are asserted by test against the constants in
`internal/service/checklist_reader.go`. Since the message is the only thing separating the two cases,
changing either string is a breaking change to this route.

**The item case is deliberately uniform.** An item that exists in a project the caller cannot see gets
the same status, code and message as an item that exists nowhere at all. A caller therefore learns only
that the item is not in the project it named — which it is already authorized to know — and cannot use
this route as an existence oracle for items in other projects.

An item that exists in this formation and simply has no history is **not** an error: it is an empty
page. That is what keeps "empty" meaning exactly "no history".

### 401

Missing, expired or malformed bearer token.

---

## Authorization

Enforced at the gateway (Traefik → Heimdall → OpenFGA), not in this service.

- **Gate of record**: `auditor` on `project:{project_uid}`. Never `viewer`, because project `viewer`
  includes `user:*` and a public project's checklist would otherwise be world-readable.
- The rule matches on method and path only
  (`charts/lfx-v2-formation-service/templates/ruleset.yaml`), so the query string plays no part in
  which object is checked.
- `item_uid` **narrows** a read the gateway has already authorized. It never selects the scope: the
  formation predicate stays in the query alongside the item predicate. Dropping it once an item is
  named would turn this into a route to any item's history in any project — and since the service
  performs no per-item authorization, that predicate is the whole of the enforcement. There is a
  repository-level test for exactly this.
- A filtered read therefore returns a strict subset of what the unfiltered read returns to the same
  caller, and cannot surface a fact the caller could not already obtain.

---

## Cost

A filtered read is served by `formation_activity_item_idx` —
`(formation_uid, item_uid, ulid DESC)`. Column order is the requirement, not a detail: formation first
so the index stays compatible with the unfiltered read's leading predicate, item second as the equality
match, and `ulid DESC` last so the cursor's ordering comes from the index rather than a sort node.

Without that index the narrowed query still walks the formation's whole feed and discards non-matching
rows to fill a page — page-and-discard relocated into Postgres, where nobody is measuring it. A test in
`internal/infrastructure/postgres/activity_repository_test.go` asserts via `EXPLAIN` that the filtered
read uses the index and does not sequentially scan `formation_activity`, on the first page and on a
paged read, so that regression fails the build rather than going unnoticed.

That gate is worth trusting only if it has been seen to fail. To re-check it, call `dropItemIndex` at
the top of `TestFilteredReadUsesTheItemIndexAndNeverScansTheFeed` and run it: the plan becomes a
`Seq Scan` plus a `Sort` and both assertions fire. Worth knowing what that proves and what it does not
— the correctness tests in the same file still **pass** without the index, returning the right rows off
a sequential scan. Wrong cost, right answer, which is why the plan is asserted separately.

Which index the *unfiltered* read uses is left to the planner and moves with the data distribution; the
same test file asserts only that adding the item index did not change that plan.

---

## Versioning

`v` stays `1`. An optional parameter that defaults to absent, on an unchanged path and method, with an
unchanged success shape and no change to any existing error body, is additive. The gateway rule does
not match on the query string, so nothing at the gateway changes either.

A consumer that sends no `item_uid` observes identical responses, ordering and cursors to those it
received before this parameter existed.
