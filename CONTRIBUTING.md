# Contributing to octo-doc

Thanks for your interest. This is a small, strict Go codebase; the bar is
readability, correctness, and test coverage over cleverness.

By participating, you agree to abide by our
[Code of Conduct](CODE_OF_CONDUCT.md). To report a security issue, follow the
[Security Policy](SECURITY.md) — please don't open a public issue.

## Prerequisites

- Go 1.26+
- Docker (for the PostgreSQL + MinIO test services)
- `golangci-lint` v2 (`make lint` expects it on `PATH`)

## Layout

```
cmd/octo-doc/        entrypoint: serve | migrate | bootstrap | health
internal/core/       dependency-free domain kernel (byte-equivalent port)
internal/service/    application logic (doc, comment, auth)
internal/storage/    MetadataStore + BlobStore (postgres, mysql, s3, memory)
internal/transport/  HTTP layer (chi router, handlers, middleware)
internal/platform/   cross-cutting: config, logging, typed errors, sluglock
assets/              overlay.js (embedded via go:embed)
```

Dependencies flow one way: **transport → service → storage**, with `core` as a
leaf and `platform` as cross-cutting support.

## Development workflow

```bash
make check        # fmt + vet + lint + test — run this before pushing
make test-race    # race detector
make cover        # coverage summary
```

To exercise the storage and e2e suites against real services:

```bash
docker run -d --name octo-pg -e POSTGRES_USER=octo -e POSTGRES_PASSWORD=octo \
  -e POSTGRES_DB=octodoc -p 55432:5432 postgres:16-alpine
docker run -d --name octo-minio -e MINIO_ROOT_USER=minioadmin \
  -e MINIO_ROOT_PASSWORD=minioadmin -p 59000:9000 minio/minio server /data
# create the bucket once, then:
make test         # the Makefile exports the OCTO_TEST_* defaults
```

Without the `OCTO_TEST_*` variables, those suites skip cleanly.

## Guidelines

- **`internal/core` is a frozen byte-equivalent port.** Changes there must keep
  `go test ./internal/core/` green (the tests pin its exact output). Read
  [docs/PORTING.md](docs/PORTING.md) before touching it.
- Keep handlers thin — validate and shape only; logic lives in services.
- Return typed errors from `internal/platform/apperr`; the HTTP layer maps them.
- Add or update tests for any behavior change. New storage behavior should be
  exercised through the contract suite so all backends stay in lockstep.
- Format with `gofmt`; the CI format check fails on unformatted files.

## Commits & pull requests

- Use clear, imperative commit subjects. Conventional Commit prefixes
  (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`) are encouraged.
- Keep PRs focused. Describe the change and how you verified it.
- CI must be green: format, vet, lint, race tests, the pg+s3 integration suite,
  and the Docker build.
