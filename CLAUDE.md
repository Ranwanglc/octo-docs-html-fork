# octo-doc — agent guide

Self-hosted, prompt-native interactive HTML docs with versioning + anchored
comments. **Go 1.26**, chi router, PostgreSQL or MySQL + S3-compatible storage. Full design
in `docs/ARCHITECTURE.md` / `docs/DESIGN.md`; the TS→Go port strategy is in
`docs/PORTING.md`.

## Commands

```bash
make build        # build bin/octo-doc
make run          # run the server (needs DATABASE_URL + S3_*)
make test         # all tests (pg/s3 suites skip without OCTO_TEST_* env)
make test-race    # tests under the race detector
make cover        # coverage summary
make lint         # golangci-lint (v2)
make check        # fmt + vet + lint + test — the local gate
```

The storage + e2e suites run against real services when `OCTO_TEST_DATABASE_URL`
and `OCTO_TEST_S3_BUCKET` (+ endpoint/creds) are set; otherwise they skip. The
`Makefile` exports sensible localhost defaults (pg on :55432, MinIO on :59000).

## Architecture

Dependencies flow one way: **transport → service → storage**, with `core` a
dependency-free leaf and `platform` as cross-cutting support.

- `internal/core/` — pure domain kernel (no I/O): aid stamping, event-log fold,
  ops, reconcile, overlay injection. **Byte-equivalent port of the original
  TypeScript implementation.**
- `internal/service/` — DocService, CommentService (per-slug lock), AuthService.
- `internal/transport/httpx/` — chi router, thin handlers, middleware.
- `internal/storage/` — `MetadataStore` (postgres) + `BlobStore` (s3) interfaces;
  `memory/` is a test fake. No driver type leaks past a store package.
- `internal/config/` — 12-factor config, parsed once (see Config below).
- `internal/platform/` — cross-cutting support: `log` (slog), typed `apperr`,
  `sluglock`.
- `assets/overlay.js` — browser code, embedded via `go:embed`, served verbatim.
- `cmd/octo-doc/` — the server binary (see Entrypoint). This repo is API-only;
  the agent client is a separate `octo-cli` project that talks to `/v1`.

## Gotchas (these are enforced — don't fight them)

- **`internal/core/` is a byte-equivalent port and must stay that way.** Every
  change must keep `go test ./internal/core/` green — the tests pin the exact
  stamped output, aid strings, fold snapshots, and op status codes. See
  `docs/PORTING.md` for the three porting traps (Math.imul 32-bit wrap,
  charCodeAt UTF-16 code units, RE2's lack of backreferences).
- **The `odoc` identifier namespace is reserved.** Stamped artifacts carry
  `data-odoc-aid`, the overlay uses `odoc-*` classes and the `window.__ODOC__`
  boot global, and the agent identity is `odoc-agent` — don't collide with these
  in doc HTML. The aid **hash** is Cyrb53 over stripped content with the attribute
  name removed before hashing, so the emitted identifier string is independent of
  the hash.
- **`internal/core/` tests are behavioral, self-contained Go tests** — no external
  fixtures. The original golden `testdata/` (generated from the now-deleted
  TypeScript) was removed once the port was complete; the tests now assert the
  observable contract directly. When you change a `core` output on purpose, update
  the pinned expectations in the corresponding `_test.go`.
- **`overlay.js` is the single source of truth**, embedded with `go:embed` in
  `assets/`. It is browser code — never reformat or transpile it.
- **Storage is a SQL database (PostgreSQL or MySQL) + S3 only.** There is no
  embedded/sqlite fallback; `STORAGE_DRIVER` selects postgres (default) or mysql.
  The two interfaces live in `internal/storage`; keep adapters behind them.
- **`golangci-lint` must pass with 0 issues** (`make lint`). Exported symbols
  need doc comments; unchecked errors and unclosed bodies are flagged.
- Use Conventional Commit subjects (lowercase, e.g. `fix: …`).

## Config

12-factor via env (`.env.example`). Parsed once in `internal/config`; no other
package reads the environment for app settings. Key vars: `DATABASE_URL`,
`S3_BUCKET`/`S3_ENDPOINT`/`S3_*`, `WRITE_TOKEN` (author auth), `ALLOW_BOOTSTRAP`,
`REPO_URL`. (No `GITHUB_*` — OAuth removed. No `PRIVATE` — docs are private by
default; access is per-doc via share codes, see `docs/AUTH.md`.)

## Entrypoint

One binary:

- `cmd/octo-doc` — the **server**. Subcommands: `serve` (default), `migrate`,
  `bootstrap`, `gc-assets`, `health`, `version`. Loads full server config (DB + S3)
  on every command except `health` and `version` (dependency-free). Version is
  stamped via `-ldflags "-X main.version=…"` (`make build`/`make release` derive it
  from `git describe`).

The repo is **API-only**: all authoring happens over the versioned `/v1` API. The
agent client CLI lives in a separate `octo-cli` project (it wraps `/v1`); it is not
built or released from this repo.

## Access control

Documents are **private by default**: the write token is the author; a per-doc
share **code** grants read+comment. Browsers carry the code as `?code=` → HttpOnly
cookie; agents/CLI send it as `Authorization: Bearer`. See `docs/AUTH.md`. The
draft slot (`docs/<hash>/draft/…`, meta `Extra["share"]`) is author-only and never
enters the immutable version numbering until `promote`.

## API

Public surface is the versioned `/v1` envelope API conforming to the OCTO spec
(`/v1/docs` incl. `/draft` + `/draft/promote` + `/share`, `/v1/comments`,
`/v1/reactions`, `/v1/agent/replies`, `/v1/admin/bootstrap`). The legacy `/api/*`
routes were removed.
