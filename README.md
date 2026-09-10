# LFX V2 Formation Service

A backend service for managing project formation checklists and other formation related activities

## How a checklist comes to exist

Nobody creates a checklist by hand. A project reaching a formation stage is what
causes one, and three paths act on that fact. They are not alternatives — they
run the same code, and what a project ends up with does not depend on which one
reached it.

| Path | Wakes on | Role |
|------|----------|------|
| Listener | a published project change | speed |
| Sweep | a timer, `RECONCILE_INTERVAL` | correctness |
| CLI | an operator | repair |

The **listener** subscribes to `lfx.project.created` and `lfx.project.updated`
and reconciles the project the message names, which is what normally puts a
checklist in place within seconds. It decides nothing itself; it turns a message
into a project reference and hands it to the same reconcile the sweep uses.

The **sweep** is the mechanism of record and the only one guaranteed to run. It
matters because the listener's transport gives no acknowledgement, no
redelivery, and nothing at all while this service is restarting — so events are
lost occasionally and by design. Everything the listener does, the sweep does
anyway; losing an event costs freshness and never a checklist. That is why the
interval is a day rather than minutes: it is the worst case when the listener is
not working, not the ordinary wait.

Every replica runs both, with no leader election. Concurrent creation is absorbed
by a uniqueness constraint on the project, so duplicate work is a no-op rather
than a race. Sweep log lines name the replica that produced them, so three
sweeps a day on three replicas reads as three replicas rather than a misbehaving
timer.

Two consequences worth knowing before debugging a missing checklist:

- **An event carrying no project stage is ignored on purpose.** Most indexed
  project documents have no stage field at all, and treating its absence as
  "no longer forming" would freeze checklists people are actively working on.
  The sweep, which asks the project service directly, settles those.
- **Nothing is created until a template is published.** `formation-cli seed` is
  a required one-time step per environment; see [Operator
  commands](#operator-commands).
- **The platform checks run on the sweep, not on an event.** They ask other
  services whether a repository, mailing list or committee exists yet, and none
  of those changes because a project document did — so an event carries no news
  about them. A resource created today shows on the checklist by the next sweep.

To tell a working listener from a dead one, look for `project event listener
summary` — it is logged on the sweep's interval and reports zero rather than
staying silent, which is the whole point of it.

## Getting Started

1. Generate the Goa API code. Generated code **is** committed in this repo (`gen/` is tracked, and the module will not build without it), so regenerate after every change to `cmd/formation-api/design/` and commit the result:

   ```bash
   make apigen
   ```

   Commit the resulting `gen/` directory along with your other changes. CI fails if `gen/` is out of date with the design.
2. Implement your service logic in `internal/service/service.go` and wire any dependencies in `cmd/formation-api/service/providers.go`.

## Local Postgres

The service applies its own schema at startup (`internal/infrastructure/postgres/schema.sql`,
embedded and run idempotently under a Postgres advisory lock), so it only needs an empty
database to connect to — no separate migration step.

Start one locally:

```bash
docker run -d --name formation-postgres \
  -e POSTGRES_HOST_AUTH_METHOD=trust \
  -e POSTGRES_DB=formation \
  -p 5432:5432 \
  postgres:16-alpine
```

The integration tests below need a second, separate database whose name ends in `_test`, which
the image does not create on its own:

```bash
docker exec formation-postgres createdb -U postgres formation_test
```

### Configuration

Credentials arrive as five discrete environment values, following the same `PGHOST`/`PGPORT`/
`PGUSER`/`PGPASSWORD`/`PGDATABASE` convention `lfx-v2-newsletter-service` uses, sourced from
the provisioned secret in every environment (`/cloudops/rds-managed/lfx-v2/formation`). There is
deliberately no single-URL form — the DSN is composed in process so the password is never part
of a value that could be logged whole.

| Variable             | Default     | Note                                                              |
|----------------------|-------------|-------------------------------------------------------------------|
| `PGHOST`             | `localhost` |                                                                   |
| `PGPORT`             | `5432`      |                                                                   |
| `PGUSER`             | *(none)*    | required                                                          |
| `PGPASSWORD`         | *(none)*    | required                                                          |
| `PGDATABASE`         | `formation` |                                                                   |
| `PGSSLMODE`          | `require`   | TLS is mandatory; set `disable` for a local container without TLS |
| `RECONCILE_INTERVAL` | `24h`       | period of the reconcile sweep; any `time.ParseDuration` value     |
| `NATS_URL`           | `nats://nats:4222` | one connection serves project lookups, the listener and the projection |

Without NATS the service still serves requests and still creates checklists
through the CLI, but it cannot list forming projects, cannot receive project
events, and cannot refresh the queue's search projection. Each of those degrades
with a log line at startup rather than a failure to boot.

Against the container above:

```bash
PGHOST=localhost PGPORT=5432 PGUSER=postgres PGPASSWORD=postgres \
PGDATABASE=formation PGSSLMODE=disable make run
```

### Operator commands

Checklist templates have no API routes, so template management is a separate
binary. It reads the same `PG*` environment as the service and applies the
embedded schema on connect, so it needs no migration step of its own.

`seed` is a required one-time step when deploying to a new environment, and
nothing runs it automatically — no service chart here ships a `Job`. Until it
has run, neither the sweep nor the listener creates anything, because no template
is published; the sweep reports that once per sweep and names the command rather
than failing per project.

```bash
make build-cli

# Publish the embedded template. Idempotent, and the first publication's
# timestamp is preserved across re-runs.
./bin/formation-cli seed

# Check the embedded template content without touching a database.
./bin/formation-cli validate

# Create one project's checklist from the published template. Idempotent.
# The listener and the sweep already do this for every forming project, so reach
# for this only to create one project's checklist out of band.
./bin/formation-cli expand <project-uid>

# Add items an existing checklist is missing, matched on key. Adds only —
# it never removes an item or resets a status, so a row dropped from a newer
# template stays put and work already done is untouched. With no argument it
# covers every checklist.
./bin/formation-cli upgrade [project-uid]
```

### Integration tests

`internal/infrastructure/postgres` gates its Postgres-backed tests on `FORMATION_TEST_DATABASE_URL`
(a plain `go test ./...` skips them). Point it at a dedicated database whose name ends in
`_test` — the tests refuse to run against anything else, and they truncate every table this
service owns before each run. The container above uses trust auth (no password), so this
command doesn't need one either:

```bash
FORMATION_TEST_DATABASE_URL="postgres://postgres@localhost:5432/formation_test?sslmode=disable" \
  make test-integration
```
