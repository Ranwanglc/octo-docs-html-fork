# octo-doc Design Notes

Motivation, runtime selection, threat model, the storage-adapter contract, and
operational procedures (backup, upgrade).

## Motivation

octo-doc makes prompt-native, commentable documents something you host yourself.
The product is "host an HTML file with comments and versions" — so it ships as a
single static Go binary that runs anywhere — a $5 VPS, a homelab box, a container
platform — with `docker compose up -d` or a one-file `octo-doc serve`, backed by
PostgreSQL or MySQL and any S3-compatible object store you already run.

## Runtime & framework selection

**Runtime: Go 1.26.** The original prototype was Node/TypeScript; the production
build is a Go rewrite. The decisive reasons:

- **A single static binary.** `go build ./cmd/octo-doc` produces one
  self-contained executable (~21 MB) with no runtime, no `node_modules`, and no
  native-module build step. Deploy is `scp` + run, or a tiny container.
- **A distroless container.** `deploy/Dockerfile` is a multi-stage build that
  copies the static binary into a `distroless/static` base — a **~25 MB** image
  with no shell, no package manager, and a minimal CVE surface.
- **`assets/overlay.js` is embedded with `go:embed`**, so the binary carries the
  browser overlay with no filesystem dependency at runtime.

The binary exposes subcommands — `octo-doc serve | migrate | bootstrap |
health` — from a single entrypoint (`cmd/octo-doc/`).

**Framework: chi (`github.com/go-chi/chi/v5`).** A thin, idiomatic router over
`net/http`'s standard `http.Handler`, so the entire app is testable via
`httptest` with no real socket and middleware composes as ordinary
`http.Handler` wrappers. The transport layer lives in `internal/transport/httpx/`.

## Threat model

octo-doc serves **untrusted, user-authored HTML with inline JS** as its whole
purpose. The security posture:

| Threat | Mitigation |
| ------ | ---------- |
| **Path traversal via slug** (`slug=../../etc`) | `safeSlug()` validates `^[A-Za-z0-9_-]{1,64}$` at every route; blob keys are additionally the SHA-256 hash of the slug, so even a bypassed slug can't escape the storage root. |
| **Parent-panel XSS / clickjacking** | CSP `frame-ancestors` (default `'none'`) + `X-Frame-Options` on every `/d/*` response. Embedding requires explicit `FRAME_ANCESTORS`. |
| **Cookie theft via same-origin user HTML** | Recommended deployment serves docs from a **separate subdomain** (`d.example.com`) than any trusted panel, so a doc's inline JS lives in its own origin and cannot read app cookies. The Caddyfile documents this two-site layout. |
| **Token leakage** | Write token travels only in the `Authorization: Bearer` header, never the URL. Token comparison is constant-time. |
| **Unbounded HTML** | `MAX_HTML_BYTES` (default 5 MiB) rejects oversized docs with 413. |
| **Write flooding** | Fixed-window rate limiter keyed by `token + client IP` on all mutating routes. |
| **Disk exhaustion** | `MAX_VERSIONS_PER_SLUG` quota knob; immutable blobs are append-only so an operator can prune old versions out of band. |
| **Server never evals user content** | HTML is stored and served as an opaque blob. The server's own CSP wrapper does not execute it; `stampAids` is pure string rewriting. |
| **Corrupt stored comments** | `safeParseList` fails soft to `[]` for display; writes go through the per-slug lock (`internal/platform/sluglock`) so a partial write can't interleave. |

What octo-doc does **not** defend against (by design): the *content* of a
document's own inline JavaScript. That JS runs in the viewer's browser on the
doc's origin — which is exactly why the separate-subdomain deployment matters.

## Storage adapter contract

The service layer depends only on two interfaces; no pgx/S3 type ever reaches a
handler. Both interfaces are defined in
[`internal/storage/store.go`](../internal/storage/store.go), with one
implementation package each: `internal/storage/postgres/` and
`internal/storage/s3/` (plus `internal/storage/memory/` for tests).

```
MetadataStore   small structured records (meta, comments, sessions, tokens)
  GetMeta/PutMeta/DeleteMeta/ListMeta
  GetComments/PutComments/DeleteComments      ← always returns a slice
  GetSession/PutSession(ttl)/DeleteSession    ← honors TTL
  GetToken/PutToken/AnyToken
  Close()

BlobStore       immutable HTML keyed by (slug, version)
  PutDoc/GetDoc/HeadDoc/ListVersions/DeleteDoc
```

octo-doc has exactly **two required backends behind these two interfaces**:
PostgreSQL or MySQL for `MetadataStore` and an S3-compatible object store for `BlobStore`.
There is no single-node fallback — this metadata/blob split is the supported
topology everywhere, which keeps the production path and the local-development
path (Postgres + MinIO) identical. The in-memory store exists only to make
`internal/core`/service tests hermetic.

### Why two stores, not one

Metadata is small, transactional, and queried (`listMeta` for the catalog).
Blobs are large, immutable, and write-once. Splitting them lets each pick the
right backend: a relational store for metadata, an object store for blobs —
each pluggable behind its interface.

## Concurrency, in depth

The per-slug lock (`internal/platform/sluglock`) is an in-process keyed mutex.
It guarantees the read-modify-write of a slug's comment list is atomic within
one process.

For **multi-instance** deployments (several app containers behind a load
balancer sharing one Postgres), the in-process lock is insufficient. Because
`sluglock` is an interface, the intended upgrade is a drop-in implementation
that wraps the comment read-modify-write in
`SELECT pg_advisory_xact_lock(advisory_key(slug))` inside a transaction. The
event log's `dedupEvents` convergence is the safety net in the meantime —
concurrent writes converge to the same fold rather than corrupting.
Single-instance is the documented, supported default.

## Backup & restore

Two backends, two backup streams — metadata in the SQL database (Postgres or
MySQL), blobs in the bucket:

```bash
# metadata + comments — use the tool matching your STORAGE_DRIVER:
pg_dump "$DATABASE_URL" > octo-doc.sql            # postgres
# mysqldump --single-transaction <db> > octo-doc.sql   # mysql
# Blobs live in the S3 bucket; use S3 versioning + lifecycle rules for retention,
# or `aws s3 sync s3://$S3_BUCKET ./backup/blobs` for a cold copy.
# Restore: psql < octo-doc.sql (or mysql < octo-doc.sql), then sync blobs back.
```

S3 lifecycle policy is the recommended way to cap blob growth: e.g. transition
objects older than 90 days to infrequent-access, or expire non-current versions
if bucket versioning is on.

## Upgrade procedure

1. Pull the new image (`docker pull`) or rebuild the binary (`go build
   ./cmd/octo-doc`).
2. `octo-doc migrate` (idempotent — all DDL is `IF NOT EXISTS`).
3. Restart. Immutable blobs and the append-only comment log mean no data
   transformation is needed across versions — old docs render unchanged.

Because documents are immutable and comments are an append-only event log,
upgrades are non-destructive by construction: there is no in-place mutation a
new version could corrupt.
