# octo-doc Architecture

octo-doc is a self-hosted, prompt-native interactive document server that
preserves the document model, URL scheme, and comment semantics of its original
TypeScript implementation byte-for-byte. It is written in Go 1.26 and ships as a
single static binary backed by PostgreSQL (metadata) and an S3-compatible object
store (blobs).

## System shape

```
┌──────────────────────── octo-doc (self-hosted) ─────────────────────┐
│                                                                       │
│   API client ──POST /v1/docs──▶  Go 1.26 app (chi router, static binary)     │
│   (Bearer token)             │                                         │
│                              │  transport/ ─▶ service/ ─▶ storage/     │
│                              │  (thin httpx) (logic)     (interfaces)  │
│                              │                                          │
│                              ├── internal/core/ (dependency-free):     │
│                              │     cyrb53.go         hash primitive      │
│                              │     stamp.go          data-odoc-aid       │
│                              │     fold.go           event-log fold       │
│                              │     events.go         eid/dedup/migrate    │
│                              │     ops.go            applyCommentOp        │
│                              │     reconcile.go      anchor reconcile       │
│                              │     render.go         overlay injection      │
│                              │     types.go          shared domain types     │
│                              │                                          │
│                              ├── internal/service/ DocService,          │
│                              │     CommentService, AuthService           │
│                              ├── internal/platform/ sluglock (per-slug  │
│                              │     write lock), config, log, apperr      │
│                              └── internal/storage/ {MetadataStore,      │
│                                    BlobStore}: postgres/ + s3/          │
│                                    (memory/ for tests)                  │
│                           assets/overlay.js embedded via go:embed       │
└───────────────────────────────────────────────────────────────────────┘
```

### Layering

Dependencies flow one way: **transport → service → storage**, with
`internal/core/` as a dependency-free domain kernel (a leaf) and cross-cutting
`internal/platform/` (`config`, `log`, `apperr`, `sluglock`). Handlers in
`internal/transport/httpx/` are thin (validate + shape); all logic lives in
services; no storage type (a pgx row, an S3 object) ever reaches a handler.
Module boundaries are ordinary Go packages exporting their public surface; there
are no import cycles.

### Storage

| Concern | Backend |
| ------- | ------- |
| Immutable version HTML + the mutable draft slot | `BlobStore` → S3-compatible (S3 / MinIO) |
| Doc metadata, comments, sessions | `MetadataStore` → PostgreSQL (pgx) |
| Per-slug write serialization | in-process keyed mutex (`internal/platform/sluglock`) |
| Author auth | `WRITE_TOKEN` env, or a `/v1/admin/bootstrap` token |
| Overlay delivery | `assets/overlay.js` embedded via `go:embed` |

## Rendering parity (byte-equivalent output)

The success criterion *"相同输入下渲染字节级等价于上游"* is met by
**porting the rendering-critical functions verbatim** into `internal/core/`
rather than rewriting them:

- `stampAids()` — stamps `data-odoc-aid="<cyrb53 hash>"` on every commentable
  artifact. Ported character-for-character from the original implementation (the
  aid hash is byte-identical; only the attribute name is octo-doc-native). Pinned
  by `go test ./internal/core/` (exact stamped HTML + aid strings) across ordinary
  and adversarial HTML.
- The event-log comment model (`snapshotAt`, `dedupEvents`, `reconcileAnchors`,
  `compactComments`) — ported verbatim.
- Overlay injection (`injectOverlayCfg`) — ported verbatim; the only change is
  that `assets/overlay.js` is embedded via `go:embed` instead of inlined at
  build time. The bytes reaching the browser are identical.

Three porting traps that would silently break byte-equivalence are documented in
[PORTING.md](./PORTING.md): `Math.imul` 32-bit wraparound (reproduced with
`uint32` arithmetic), `charCodeAt` operating on UTF-16 code units (not Go runes
or bytes), and RE2's lack of backreferences (no `\1` in the Go `regexp`
package).

The single deliberate divergence: `eventEid()` for one-shot events used
`Math.random()` upstream; octo-doc uses a monotonic counter + high-res time.
This only affects the *uniqueness suffix* of non-idempotent event ids, never
the fold result — `dedupEvents` keys on the id, and idempotent events keep
their deterministic ids unchanged.

## Data model

Unchanged from upstream:

- **Document**: `slug` + monotonically increasing integer `version` →
  immutable HTML blob. A republish of the same slug always gets `max(version)+1`.
- **URL**: `/d/<slug>/v/<version>` (preserved). Plus `/export` and `/fork`.
- **Comments**: an append-only **event log** per slug. Each version is a
  snapshot — reading "as of version N" folds events with `at_version <= N`.
  Mutations append events; they never overwrite. See `internal/core/fold.go`.

### Storage records

| Store               | Key            | Value                                            |
| ------------------- | -------------- | ------------------------------------------------ |
| `MetadataStore.meta`     | slug      | `{ title, slug, versions: [{n, created}] }`      |
| `MetadataStore.comments` | slug      | the full event-log comment array                 |
| `MetadataStore.sessions` | sid       | `{ login, avatar_url, name, created }` (+ TTL)    |
| `MetadataStore.tokens`   | token     | `{ token, created, label }`                      |
| `BlobStore`         | (slug, version) | immutable stamped HTML                          |

## API specification

All JSON endpoints live under **`/v1`** (the single current API version) and
speak the OCTO wire contract: a successful response wraps its payload in a
top-level `data`; a list adds a sibling `pagination`; an error returns a
top-level `error` object `{ code, message, details?, hint? }` whose `code` is
one of a fixed enum (`VALIDATION_ERROR`, `AUTH_REQUIRED`, `FORBIDDEN`,
`NOT_FOUND`, `CONFLICT`, `PAYLOAD_TOO_LARGE`, `UNSUPPORTED_MEDIA_TYPE`,
`RATE_LIMITED`, `UPSTREAM_UNAVAILABLE`, `INTERNAL_ERROR`). Timestamp fields carry
the `_at` suffix on the wire (`created_at`); the byte-equivalence-locked `core`
kernel keeps its `created` field internally and is remapped to `created_at` at
the transport DTO boundary. The `/d/:slug/v/:version` document URLs are not part
of `/v1` — they return browser HTML, not the JSON envelope.

### Reads (capability-gated — private by default)

Documents are private by default. Each read resolves a capability from the request
(write token = author, per-doc share code = reader; else none → **404**, existence
hidden). Browsers present the code as `?code=` once and it is exchanged for an
HttpOnly cookie; agents/CLI send it as `Authorization: Bearer`. See docs/AUTH.md.

| Method | Path | Description |
| ------ | ---- | ----------- |
| `GET`  | `/v1/ping` | `{ data: { ok, service: "octo-doc" } }` health/identity marker (unauthed) |
| `GET`  | `/healthz` | `{ data: { ok } }` liveness for orchestrators (unversioned, unauthed) |
| `GET\|HEAD` | `/d/:slug/v/:version` | rendered doc with overlay injected (reader) |
| `GET\|HEAD` | `/d/:slug/draft` | rendered draft, overlay in draft mode (**author only**) |
| `GET`  | `/d/:slug/v/:version/export` | doc + comment banner, `Content-Disposition: attachment` (reader) |
| `GET`  | `/d/:slug/v/:version/fork` | doc + comments, overlay in read-only fork mode (reader) |
| `GET`  | `/v1/docs/:slug/versions` | `{ data: { slug, title, versions: [{n, created_at}] } }` (reader) |
| `GET`  | `/v1/comments?slug=&version=` | `{ data: [...], pagination }` folded snapshot (reader; `version=all` for full history) |
| `GET`  | `/` | neutral landing page (no catalog, unauthed) |
| `GET`  | `/me` | owner-only doc catalog (redirects others) |

### Comment mutations (reader capability required)

| Method | Path | Description |
| ------ | ---- | ----------- |
| `POST`   | `/v1/comments` | create a comment or reply |
| `PATCH`  | `/v1/comments` | re-anchor |
| `DELETE` | `/v1/comments?slug=&id=&version=` | soft-delete |
| `POST`   | `/v1/reactions` | toggle an emoji reaction |

Commenting requires at least a **reader** capability (the doc's share code, or the
author write token) — a default-private doc cannot be commented on anonymously.
Comment identity is still anonymous (no login provider); the author/owner checks on
PATCH/DELETE are the seam a future Octo unified login activates.

### Author endpoints (write token, or the author cookie in a browser)

| Method | Path | Description |
| ------ | ---- | ----------- |
| `POST`   | `/v1/docs` | publish directly (JSON `{slug, version, html, meta, comments}` or multipart); auto-increments version when omitted |
| `PUT`    | `/v1/docs/:slug/draft` | save/overwrite the mutable draft slot |
| `POST`   | `/v1/docs/:slug/draft/promote` | promote the draft to a new immutable version |
| `POST`   | `/v1/docs/:slug/share` | mint/rotate the per-doc read+comment code → `{ code, url }` |
| `DELETE` | `/v1/docs/:slug/share` | revoke the share code |
| `POST`   | `/v1/agent/replies` | agent posts a reply + verdict (✅/🟡/❓) |
| `DELETE` | `/v1/docs/:slug` | delete all versions + comments |
| `DELETE` | `/v1/comments?slug=&all=1` | wipe all comments for a slug |
| `POST`   | `/v1/admin/bootstrap` | mint the first write token (then 409s) |

Author endpoints accept the write token as `Authorization: Bearer` (CLI) or, for
the browser Publish/Share buttons, the author credential via the per-doc cookie.

### Viewer sessions

`GET /v1/auth/me` (reports the current viewer; anonymous → `identity: null`) and
`POST /v1/auth/logout` (clears a session). There is no built-in login provider
yet; the session machinery (`sessions` table, `AuthService.CreateSession`) is the
seam a future Octo unified login plugs into.

## Concurrency

Per-slug comment writes are serialized by `internal/platform/sluglock` — an
in-process keyed mutex that makes `read → applyCommentOp → write` atomic for a
given slug. This is correct
for the default **single-instance** deployment. The event log additionally
converges under concurrent writes via `dedupEvents` (stable event ids), so even
races that the mutex doesn't cover (e.g. future multi-instance) degrade to
last-write-wins-per-event rather than corruption. `sluglock` is an interface, so
multi-instance horizontal scaling can swap the in-process lock for a Postgres
advisory-lock implementation, documented in [DESIGN.md](./DESIGN.md).

## Request lifecycle (publish)

```
POST /v1/docs  (Authorization: Bearer <token>, multipart or JSON)
  ├─ requireWriteAuth         constant-time token check
  ├─ size cap check           (MAX_HTML_BYTES, default 5 MiB)
  ├─ next version = max(blobStore.listVersions)+1   (if not explicit)
  ├─ stampAids(html)          identity-stamp artifacts (verbatim port)
  ├─ blobStore.putDoc         immutable write + head-verify
  ├─ metaStore.putMeta        monotonic versions[]
  └─ commentStore.publish_merge   reconcile anchors + merge local comments
     → { ok, slug, version, url: "/d/<slug>/v/<n>", size, aids, mergedComments }
```

See [DESIGN.md](./DESIGN.md) for the runtime/framework selection rationale,
threat model, adapter contract, and backup/upgrade procedures.
