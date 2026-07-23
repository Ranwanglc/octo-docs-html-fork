# octo-doc `/v1` API reference

The complete HTTP surface the skill drives. octo-doc is **API-only**; every
authoring step is one of these calls. `scripts/octo.sh` wraps them — this file is
the ground truth behind that wrapper.

Base path is `/v1`. All JSON responses use an envelope:

```json
{ "data": <payload> }            // success
{ "error": { "code": "...", "message": "...", "details": {...}, "hint": "..." } }
```

Errors carry an HTTP status + a stable `code` (e.g. `invalid_slug`,
`html_required`, `unsupported_media_type`, `payload_too_large`, `not_found`,
`rate_limited`). A private doc a caller can't see returns **404** (never confirms
existence).

## Authentication

Two credentials, both sent as `Authorization: Bearer <value>`:

| Credential | Role | Can |
| --- | --- | --- |
| **write token** | **author** | everything: publish, draft, promote, delete, share, upload/delete assets, agent replies |
| **share code** | **reader** | read published versions; comment; react |
| _(none)_ | — | nothing → 404 |

Browsers instead carry the share code as `?code=<code>` on a `/d/...` URL, which
the server swaps for an HttpOnly cookie. API clients just send the Bearer header.

Mint the first write token on a fresh server:

```bash
curl -sX POST "$BASE/v1/admin/bootstrap" | jq -r .data.token   # → the write token
```

`bootstrap` only works while the server has no static `WRITE_TOKEN` configured and
hasn't been bootstrapped yet (`ALLOW_BOOTSTRAP`).

## Documents

### `POST /v1/docs` — publish (author)
Save HTML and create version 1 (or the next version) in one step.

```bash
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"slug":"demo","title":"Demo","html":"<html><body><h1>Hi</h1></body></html>"}' \
  "$BASE/v1/docs"
# data: { "slug":"demo", "version":1, "url":"/d/demo/v/1", "doc_id":"...", "share_url":"https://...", "registered":true, "status":"published", "size":..., "aids":..., "merged_comments":... }
```

Body: `{slug, html, title?, version?, comments?, mount_type?, group_no?, thread_id?}`. `slug` is kebab-case
(`^[a-zA-Z0-9_-]{1,64}$`). HTML is capped at `MAX_HTML_BYTES` (413 over limit).

For a registerable `group` or `space` mount, the service writes the immutable
HTML version exactly once, then retries only docs-backend registration for a
short bounded window. Registration success includes the canonical `doc_id` and
`share_url`; both a new row (`created:true`) and an existing idempotent row
(`created:false`) return `registered:true,status:"published"`. If registration
still fails, the publish response is `registered:false,status:"registration_failed"`:
the HTML version already exists and callers must not report ordinary publish
success or retry by publishing the HTML again.

### `PUT /v1/docs/{slug}/draft` — save the mutable draft (author)
Overwrites the single draft slot without minting a version. Body: `{html, title?}`.
The draft renders at `/d/{slug}/draft` (author-only).

### `POST /v1/docs/{slug}/draft/promote` — freeze the draft (author)
Promotes the current draft to the next immutable version. Optional body `{title}`.
Returns the same shape as publish.

### `GET /v1/docs/{slug}/versions` — list versions (reader)
`data: { slug, title, versions: [{ n, created }] }`.

### `DELETE /v1/docs/{slug}` — unpublish (author)
Deletes all versions, the draft, comments, and assets for the slug.

## Sharing

### `POST /v1/docs/{slug}/share` — mint / rotate the code (author)
`data: { slug, code, url }` where `url` is `.../d/{slug}/v/{latest}?code=<code>`.
Re-running rotates the code (old links stop working).

### `DELETE /v1/docs/{slug}/share` — revoke (author)
`data: { slug, revoked: true }`.

## Comments & reactions

### `GET /v1/comments?slug=<slug>&version=<n|all>` — list (reader)
`version=all` returns full cross-version history; a number returns that version's
folded snapshot. Response is a **list envelope** — `{ "data": [...],
"pagination": { "total", "page", "page_size" } }` — where each item is a comment
snapshot:

```json
{ "id":"...", "author":{"login":"..."}, "created_at":"<iso>", "version":1,
  "anchor":{...}, "text":"...", "status":"open|applied|...",
  "replies":[...], "reactions":{"👍":["login"]}, "deleted":false }
```

Note the wire field is **`created_at`** (the stored form is `created`).

### `POST /v1/comments` — create a comment or reply (reader)
Body: `{slug, text, version?, anchor?, parent_id?}`.
- `anchor` binds a top-level comment (see `references/anchoring.md`); omit for a
  doc-level comment.
- `parent_id` makes it a reply instead.

`data: { id }`.

### `POST /v1/agent/replies` — agent reply + verdict (author)
Body: `{slug, parent_id, text, status?, applied_in?}` where `status` is
`applied | partial | question`. Flips the parent comment's status and drops a
status emoji (✅ / 🟡 / ❓).

### `PATCH /v1/comments` — re-anchor (reader)
Body: `{slug, id, anchor, version?}`. Resets an element comment onto a new anchor.

### `DELETE /v1/comments?slug=<slug>&id=<id>&version=<n>` — soft-delete (reader)

### `POST /v1/reactions` — toggle a reaction (reader)
Body: `{slug, comment_id, emoji, version?}`. Reacting again with the same
`(login, emoji)` toggles it off.

## Media assets

Large binary media (images, audio/video, PDF) is hosted per-doc rather than
base64-inlined. See `references/authoring.md`.

### `POST /v1/docs/{slug}/assets` — upload (author, multipart)
Field `file`. The bytes are content-addressed (SHA-256); the MIME is sniffed and
checked against `ASSET_MIME_ALLOW`. `data: { slug, sha256, mime, size, url }`.
Reference `url` (`/d/{slug}/assets/<sha>`) from the doc's HTML.

```bash
curl -H "Authorization: Bearer $TOKEN" -F file=@chart.png \
  "$BASE/v1/docs/demo/assets"
```

### `GET /v1/docs/{slug}/assets` — list (reader)
`data: { slug, assets: [{ sha256, mime, size, original_name, created, url }] }`.

### `DELETE /v1/docs/{slug}/assets/{sha256}` — delete (author)

### `GET /d/{slug}/assets/{sha256}` — serve bytes (reader)
Served under a locked-down CSP (`default-src 'none'; sandbox`) + `nosniff`,
`immutable` cache, and Range support. This is what a doc's `<img>/<video>/<object>`
points at.

## Rendered docs (browser)

- `GET /d/{slug}/v/{version}` — a published version with the comment overlay.
- `GET /d/{slug}/draft` — the draft (author-only).
- `GET /d/{slug}/v/{version}/{export|fork}` — self-contained export / fork copy.

## Health & identity

- `GET /healthz` — liveness (root, not under /v1).
- `GET /v1/ping` — `data: { service: "octo-doc", ... }`.
- `GET /v1/auth/me` — the caller's viewer identity.
