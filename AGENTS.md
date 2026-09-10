# AGENTS.md

Cross-tool entry point for agents working in this repository.
[`CLAUDE.md`](CLAUDE.md) is authoritative for commands, layout, and the
conventions below.

## Read first

- [`CLAUDE.md`](CLAUDE.md) — commands, layout, and the constraints that are easy
  to break.
- [`README.md`](README.md) — configuration, local Postgres, operator commands,
  and how a checklist comes to exist.

## Invariants

- `gen/` is committed and generated. Run `make apigen` after any change under
  `cmd/formation-api/design/`; never hand-edit generated files.
- `make check` and `go test -race ./...` must pass before committing.
- Every commit needs DCO signoff (`git commit -s`).
- `internal/service` must not import `internal/infrastructure`. Wire adapters in
  `cmd/formation-api/service/providers.go` and pass them as ports.
- A listener, a daily sweep, and the CLI all reconcile through one code path.
  Do not add a second place that decides whether a project gets a checklist.
- Inbound events are best-effort with no redelivery. The sweep is what makes any
  outcome correct; nothing may depend on an event arriving.
- An absent or unrecognised project stage means "leave it alone", never "left
  formation".
- Missing infrastructure degrades with a log line. It must not stop the service
  from starting.
