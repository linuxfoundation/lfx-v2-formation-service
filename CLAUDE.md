# CLAUDE.md

Guidance for Claude Code (claude.ai/code) working in this repository.

> **Central LFX skills:** start with `lfx-skills:lfx` for cross-repo routing and
> "where does X live" questions, then `lfx-skills:lfx-platform-architecture` for
> platform composition, NATS/KV ownership, and access-check flows. This repo owns
> formation checklist behaviour; treat repo-local truth as authoritative over the
> cross-service description. If the plugin is missing, install with
> `/plugin marketplace add linuxfoundation/lfx-skills` then
> `/plugin install lfx-skills@lfx-skills`.

## What this service is

A Go service that keeps one formation checklist per project in step with that
project's stage. Postgres holds the checklists; NATS carries project lookups,
inbound project events, and the search projection the queue screen reads.

## Commands

```bash
make deps            # install tools
make apigen          # regenerate Goa code — required after editing cmd/formation-api/design/
make build
make test            # go test ./...
make lint
make check           # fmt + lint + license headers + vet — run before every commit
make build-cli       # ./bin/formation-cli
```

`gen/` **is** committed and the module will not build without it. Never hand-edit
it; run `make apigen` and commit the result. CI fails if it is stale.

Race and integration coverage:

```bash
go test -race ./...
FORMATION_TEST_DATABASE_URL="postgres://postgres@localhost:5432/formation_test?sslmode=disable" \
  make test-integration     # skipped without the variable; database name must end in _test
```

## Layout

| Path | Holds |
|------|-------|
| `cmd/formation-api/design/` | Goa API design; the source of `gen/` |
| `cmd/formation-api/service/providers.go` | all dependency wiring and startup degradation |
| `cmd/formation-cli/` | operator commands: seed, validate, expand, upgrade |
| `internal/domain/model/` | entities and the project stage mapping |
| `internal/domain/port/` | interfaces the service depends on |
| `internal/service/` | use cases — the only place business decisions belong |
| `internal/infrastructure/{postgres,nats,mock}/` | adapters implementing the ports |

Dependencies point inward. `internal/service` must not import
`internal/infrastructure`; wire concrete adapters in `providers.go` and pass them
as ports. Wire formats — decoding another service's event, shaping a published
message — are infrastructure, not use cases.

## The rules that are easy to break

**One reconcile, three triggers.** A listener (project events), a daily sweep,
and the CLI all create checklists, and all three call the same
`Reconciler.ReconcileProject`. Never add a second place that decides whether a
project should have a checklist — two implementations agree the day they are
written and drift afterwards, and the drifted one is the fast path nobody sweeps
behind. See the README section "How a checklist comes to exist".

**The sweep is correctness; events are speed.** Inbound events use core NATS:
no acknowledgement, no redelivery, nothing delivered while the pod restarts.
Anything that must be true has to be reachable by the sweep. Never make a
behaviour depend on an event arriving.

**An unrecognised or absent project stage means "leave it alone".** It does not
mean the project left formation. Most indexed project documents carry no stage
at all, and the other reading freezes checklists people are working on.

**Degrade, do not refuse to start.** A missing NATS connection, a failed
subscription, or an absent template each cost a capability and are logged once
at startup. None of them may prevent the service from serving requests.

**Every replica runs everything.** No leader election. Concurrent creation is
absorbed by the uniqueness constraint on the project, so new work must be safe
to run several times at once.

## NATS

Subjects and the queue group live in `internal/infrastructure/nats/subjects.go`,
each recorded with where it came from. Keep that convention.

- Consumed: `lfx.project.created`, `lfx.project.updated`, queue group
  `formation-service-project-events`. Deletion is deliberately not consumed.
- Published: `lfx.index.formation`. The `deleted` action carries the object UID
  as a **bare string** in `data`, unlike every other action, which carries an
  encoded body.
- Requested: `lfx.projects-api.*` for project lookups.

Subscriptions extract the publisher's trace context and start a consumer span,
matching committee and project service. Handlers recover from panics, and a
handler failure is counted and dropped — there is nobody to return an error to.

## Before committing

`make check` and `go test -race ./...` must both pass. Commits need DCO signoff
(`git commit -s`).

See [`README.md`](README.md) for configuration, local Postgres, and operator
commands.
