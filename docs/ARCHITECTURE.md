# octo-doc Architecture

octo-doc is a self-hosted, prompt-native interactive document server that
preserves the document model, URL scheme, and comment semantics of its original
TypeScript implementation byte-for-byte. It is written in Go 1.26 and ships as a
single static binary backed by PostgreSQL or MySQL (metadata) and an S3-compatible object
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
| Doc metadata, comments, sessions | `MetadataStore` → PostgreSQL (pgx) or MySQL |
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

Documents are private by default. Each read resolves a capability from the
request, ordered `None < Read < Comment < Edit < Manage` (the doc creator /
`superAdmin` / `doc_member` admin = Manage; a `doc_member` writer = Edit; a
commenter = Comment; a per-doc share code or `doc_member` reader = Read; else
None → **404**, existence hidden). Browsers present the code as `?code=` once and
it is exchanged for an HttpOnly cookie; agents/CLI send it as
`Authorization: Bearer`. The four-role `doc_member` append-v1 encoding, its
mandatory runtime marker, and the legacy `meta.grants` fallback boundary are in
docs/AUTH.md.

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

### Comment mutations (Comment capability required)

| Method | Path | Description |
| ------ | ---- | ----------- |
| `POST`   | `/v1/comments` | create a comment or reply |
| `PATCH`  | `/v1/comments` | re-anchor |
| `DELETE` | `/v1/comments?slug=&id=&version=` | soft-delete |
| `POST`   | `/v1/reactions` | toggle an emoji reaction |

Commenting requires at least the **Comment** capability (a `doc_member` commenter
or higher). A share code alone is **Read**, so it does not grant commenting — a
default-private doc cannot be commented on anonymously.
Comment identity is still anonymous (no login provider); the author/owner checks on
PATCH/DELETE are the seam a future Octo unified login activates.

### Author endpoints (write token, or the author cookie in a browser)

| Method | Path | Description |
| ------ | ---- | ----------- |
| `POST`   | `/v1/docs` | publish directly; returns the HTML version plus `doc_id`, `share_url`, `registered`, and `status` |
| `PUT`    | `/v1/docs/:slug/draft` | save/overwrite the mutable draft slot |
| `POST`   | `/v1/docs/:slug/draft/promote` | promote the draft to a new immutable version |
| `POST`   | `/v1/docs/:slug/share` | mint/rotate the per-doc **read** share code → `{ code, url }` |
| `DELETE` | `/v1/docs/:slug/share` | revoke the share code |
| `GET`    | `/v1/docs/:slug/grants` | list direct grants (creator synthesized as the leading author row) |
| `PUT`    | `/v1/docs/:slug/grants` | grant a uid `reader\|commenter\|writer` (upsert); admin/creator refused |
| `DELETE` | `/v1/docs/:slug/grants/:uid` | revoke a uid's grant; creator/admin refused |
| `POST`   | `/v1/agent/replies` | agent posts a reply + verdict (✅/🟡/❓) |
| `POST`   | `/v1/agent/element/get` | return one stamped element by AID; body `version` is an integer, where `0` means latest |
| `POST`   | `/v1/agent/element/replace` | replace one stamped element and publish a new version; body `base_version` is an integer, where `0` means latest |
| `DELETE` | `/v1/docs/:slug` | delete all versions + comments |
| `DELETE` | `/v1/comments?slug=&all=1` | wipe all comments for a slug |
| `POST`   | `/v1/admin/bootstrap` | mint the first write token (then 409s) |

Author endpoints accept the write token as `Authorization: Bearer` (CLI) or, for
the browser Publish/Share buttons, the author credential via the per-doc cookie.

Comment mutation JSON accepts a non-negative integer, a decimal string, a
`"v<N>"` string, or `"latest"` for `version`; omitted or `null` means the
endpoint default. Comment list queries accept an integer, `v<N>`, `all`, or an
omitted value. Element get/replace use integer fields only (`0` = latest).

Element replacement requires `new_html` to contain exactly one safe root. The
root must either be a normally stamped artifact tag or carry
`class="odoc-artifact"`; otherwise the request returns `400 VALIDATION_ERROR`
with detail code `new_html_root_not_addressable`. The replacement root keeps the
requested AID for that immediate publish only. Reconciliation accepts it only
when the AID is unique and refreshes the tag fingerprint in the same comment
mutation; later plain publishes re-stamp normally. Reused or ambiguous AIDs are
marked lost rather than guessed, so this endpoint does not promise a durable pin.

`iframe` remains a non-void element for lookup and replacement boundaries, so
fallback content is inside the addressed outer HTML. For compatibility with
already-published documents, reconciliation also computes the historical alias
`aidFor("iframe", "", attrs)`. That alias is never emitted and never used for
element boundaries: a unique alias migrates to the canonical AID, while an
ambiguous or reused alias is marked lost.

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
POST /v1/docs  (Authorization: Bearer *** multipart or JSON)
	Canonical create: `{idempotency_key, html, ...}` with no `slug`. The server
	synchronously obtains a doc ID before writing and stores everything under it.
	A request carrying `slug` addresses that exact existing local ref instead;
	an unknown ref returns 404 and cannot create a document.
  ├─ requireWriteAuth         constant-time token check
  ├─ size cap check           (MAX_HTML_BYTES, default 5 MiB)
  ├─ canonical create registration (before local writes; mounted or unmounted)
  ├─ next version = max(blobStore.listVersions)+1   (if not explicit)
  ├─ stampAids(html)          identity-stamp artifacts (verbatim port)
  ├─ blobStore.putDoc         immutable write + head-verify
  ├─ metaStore.putMeta        monotonic versions[]
  ├─ commentStore.publish_merge   reconcile anchors + merge local comments
  └─ docs-backend notification  after persistence; bounded retries
     → { slug, version, url, doc_id, share_url, registered, status, size, aids, merged_comments }
```

`created:false` from docs-backend is an existing-row success. Canonical creation
fails closed before local writes when registration is unavailable; retrying the
same idempotency key safely resumes or returns the existing result. Existing
legacy refs are edited in place and are not newly registered.

**Rollout prerequisite:** Mininglamp-OSS/octo-docs-backend#129 must be merged
and deployed before HTML PR #24 is deployed, because thread registration
depends on that backend contract.

See [DESIGN.md](./DESIGN.md) for the runtime/framework selection rationale,
threat model, adapter contract, and backup/upgrade procedures.
