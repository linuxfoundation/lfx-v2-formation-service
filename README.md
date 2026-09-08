# LFX V2 Formation Service

A backend service for managing project formation checklists and other formation related activities

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
| `RECONCILE_INTERVAL` | `15m`       | period of the reconcile loop; any `time.ParseDuration` value      |

Against the container above:

```bash
PGHOST=localhost PGPORT=5432 PGUSER=postgres PGPASSWORD=postgres \
PGDATABASE=formation PGSSLMODE=disable make run
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
