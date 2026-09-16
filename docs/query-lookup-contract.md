# Query Lookup Contract — Formation Service

This document is the authoritative reference for what the formation service reads from the
[query service](https://github.com/linuxfoundation/lfx-v2-query-service) when resolving the
checklist rows the platform can answer for itself.

**Update this document in the same PR as any change to `internal/infrastructure/query`.**

This is the service's only outbound HTTP dependency. Everything else it reads, it reads over NATS.
Nothing is asked of the query service that it does not already offer — no new endpoint, no new
parameter, no schema change — and nothing is asked of the committee or mailing-list services at all,
because both already publish their resources with the project reference this filters on.

---

## The question

> How many resources of type *X* does `project:<uid>` have, and give me one of them.

Both halves are needed. The count is compared against the row's `min_count`, so a row asking for two
is not satisfied by the first hit. The reference is what turns a "Create committee" row into "Open
committee" pointing at the real one.

Nothing else is asked. The read is scoped to existence and counts.

---

## Calls

Two calls per lookup, in this order.

### 1. Count

```http
GET /query/resources/count?v=1&type={indexedType}&parent=project:{projectUID}
Authorization: Bearer {serviceIdentityToken}
```

```json
{ "count": 3, "has_more": false }
```

`has_more` means the count is a floor rather than an exact figure, which is harmless here: the
thresholds compared against are one or two, and a floor that clears the minimum clears it.

### 2. Reference

Issued **only when the count is non-zero**. There is nothing to name otherwise, and skipping it
matters — the zero case is the common one on every project that has not done the work yet, so
issuing a second call for it would double the sweep's load on the read layer for no possible answer.

```http
GET /query/resources?v=1&type={indexedType}&parent=project:{projectUID}&page_size=1
Authorization: Bearer {serviceIdentityToken}
```

```json
{ "resources": [ { "type": "committee", "id": "830513f8-..." } ] }
```

Only `type` and `id` are read. `data` is never parsed.

Both calls carry identical filters. That is what makes the count a valid predicate for the list: the
query service filters both by the same principal, so a count of three followed by an empty list can
only be a race and never a different question.

No `sort` is sent. The query service's default ordering is already a total order, so the first hit
names the same resource on every call while the underlying set is unchanged — imposing an ordering
here could only disagree with it.

---

## Type mapping

| Template `platform_check.resource_type` | Queried `type` |
|---|---|
| `committee` | `committee` |
| `mailing_list` | `groupsio_mailing_list` |
| `repository` | *none — no lookup is registered* |

`groupsio_mailing_list` is the individual list. `groupsio_service` is the project's Groups.io
presence and is **not** what this counts: a service can exist with no lists in it, and resolving the
row against one would call the work done while it visibly is not.

`repository` has no entry because no service in the platform owns repositories and the query service
indexes none. That row is manual from template version 2 onward, and existing rows are corrected by
the migration in the additive tail of `internal/infrastructure/postgres/schema.sql`.

This mapping is the only place in the service that knows what the query service calls things. The
template keeps the platform-neutral name.

---

## Identity

The reads are made as this service, not as the caller whose request happened to trigger them — the
reconcile sweep has no caller, so there is no bearer token to forward even in principle.

The flow is an OAuth2 client-credentials grant with a private-key JWT client assertion (RS256),
wired in `cmd/formation-api/service/providers.go` from these environment variables:

| Variable | Required | Notes |
|---|---|---|
| `QUERY_SERVICE_URL` | no | Unset disables the lookups entirely — see below |
| `M2M_AUTH_CLIENT_ID` | once the URL is set | |
| `M2M_AUTH_PRIVATE_KEY` | once the URL is set | RSA private key, PEM form |
| `M2M_AUTH_DOMAIN` | once the URL is set | Auth0 tenant |
| `M2M_AUTH_AUDIENCE` | no | |

The query service filters every result against the calling principal, which for a
client-credentials token is the client id. The grant the identity needs is read access to the
committees and mailing lists of the projects the sweep asks about; it is held through the same
mechanism that already gives service clients project-level read access, and needs no new
authorization model surface.

**A result the identity may not see is indistinguishable from a resource that does not exist.** That
is correct behaviour for an access-filtered read layer, and it is why these lookups are enabled per
environment only once the identity is working: a half-granted identity reports projects as having
done no work rather than reporting an error.

### Unconfigured is a supported state

With `QUERY_SERVICE_URL` unset, the lookup registry is built empty, no outbound call is made, and
every platform row is reported unanswerable — exactly the behaviour the service had before these
lookups existed. That is the deliberate default, and it is what lets the identity be granted and the
feature be enabled one environment at a time through values rather than through a release.

A URL set with incomplete credentials is refused at startup instead. It is not a third state: it
would read the filtered layer as nobody, and record every project as having done no work.

---

## Error mapping

The resolution pass branches on exactly one thing — whether the error is `domain.ErrNotFound` — and
the two branches mean very different things in the sweep's report.

| Outcome | Adapter returns | Report | Meaning |
|---|---|---|---|
| `200`, `count = 0` | `domain.ErrNotFound` | `Pending` | Work still to do. The ordinary case |
| `200`, count below the row's `min_count` | `(count, ref, nil)` | `Pending` | Found some, not enough |
| `200`, count satisfies, list returns a resource | `(count, ref, nil)` | `Advanced` | Resolved |
| `200`, count satisfies, list returns empty | `domain.ErrNotFound` | `Pending` | A race between the two calls. Retried next sweep; not a fault |
| `401` / `403` | `domain.ErrForbidden` | `Failed` | The identity stopped being accepted |
| other `4xx` | a non-`ErrNotFound` error | `Failed` | A malformed call — a bug in this adapter |
| `5xx`, timeout, connection refused | a non-`ErrNotFound` error | `Failed` | The layer is unreachable |

The `401`/`403` row is why this taxonomy is written down. An identity that stops being accepted
returns it on every call, and mapped to not-found that would read as every project in the platform
having done no work — silently, on every sweep, with no error anywhere. Mapped to a fault it shows
up as `platform_check_failed` rising on the `reconcile sweep finished` log line, which is the one
signal that tells the two apart. Read that line's three platform counts together: `platform_resolved`
rising and `platform_unanswerable` falling is progress, but `platform_unanswerable` falling while
`platform_check_failed` rises is not.

Every failure leaves the row untouched and is retried by the next sweep. Nothing depends on one pass
succeeding.

---

## Constraints

| Constraint | Why |
|---|---|
| Never parse `data` | The read is scoped to existence and counts. Parsing contents would couple this service to two donor schemas and widen what the identity is used for |
| Never call without `parent` | An unscoped query is a general search, which is not what the identity is for |
| Never reuse the identity for anything else | It is granted for this |
| Never treat an empty result as an error | It is the ordinary state of work still to do |
| Never retry within a pass | The sweep is the retry. A pass that retried until it won would fight the person's edit that resolution deliberately yields to |

---

## Staleness

Resolution reads an index rather than asking each owning service, and that carries one accepted
exposure. A resource that exists but is not yet indexed leaves the row alone and corrects itself on a
later sweep, which is harmless. The other direction does not correct itself: a resource deleted
before the index catches up can advance a row against nothing, and because a platform check only ever
moves a row forward, no later sweep walks it back.

The window is small, and a staff writer can always set the row by hand — which is also the remedy for
the related limitation that a row can be resolved by the wrong instance of the right type, since the
lookup matches on type and project rather than on which particular committee or list the row meant.
