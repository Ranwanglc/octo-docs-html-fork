<div align="center">

# octo-docs-html

**Self-hosted, prompt-native interactive HTML documents — Octo-integrated, with creator-owned access, anchored inline commenting, and immutable versioning.**

[![Go 1.26](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[Quick start](#quick-start) ·
[How it works](#how-it-works) ·
[Octo integration](#octo-integration) ·
[Configuration](#configuration) ·
[Self-hosting](docs/SELF_HOSTING.md) ·
[Architecture](docs/ARCHITECTURE.md) ·
[Contributing](CONTRIBUTING.md)

</div>

---

octo-docs-html turns a prompt into a self-contained interactive HTML document —
models, SVG diagrams, simulations, explainers, RFCs — publishes it at a stable
URL, and lets reviewers leave comments anchored to the **text or the artifact**
they're looking at. Every publish is an immutable version; comments re-anchor
across versions. It runs as a single static Go binary backed by PostgreSQL (or
MySQL) plus any S3-compatible object store, and slots into the **Octo** platform:
docs are owned by their **creator's Octo identity**, register into the web-docs
sidebar, and push comment events back to Octo IM.

> **Heritage.** This is the Octo-integrated build. It descends from the upstream
> `octo-doc` project (the binary name remains `octo-doc`; the Go module path is
> now `github.com/Mininglamp-OSS/octo-docs-html`) and layers
> Octo identity, per-uid grants, MySQL storage, doc-binding, docs-backend
> registration, and comment-event webhooks on top.

## Features

- **Prompt-native documents.** Authored with an AI agent over the versioned `/v1`
  HTTP API; the doc is real, self-contained HTML — not a proprietary format.
- **Creator-owned access.** Ownership is by **creator uid** — the real user's Octo
  Login, a bot's owner uid, or an Octo superAdmin. Write tokens are **not** an auth
  source anymore. A per-doc share **code** grants read + comment; per-uid
  `doc_grants` add named readers. No credential → `404` (existence hidden). See
  [docs/AUTH.md](docs/AUTH.md).
- **Anchored commenting.** Comments attach to highlighted text *or* to a stamped
  artifact (image, SVG, canvas, chart) by content-hash identity, so they survive
  edits and re-anchor across versions.
- **Immutable versioning.** `slug` + monotonic `version` → write-once HTML at
  `/d/<slug>/v/<n>`; the comment history is an append-only event log.
- **Signed inline assets.** Media sub-resource URLs (`/d/<slug>/assets/<sha256>`)
  are HMAC-signed so a browser's native `<img>`/`<video>` load — which can't carry
  an auth header — is authorized by a short-lived per-asset signature.
- **Octo-native.** Optional login via octo-server, bot-token auth, a doc-binding
  channel that resolves per-uid visibility, registration into the web-docs
  sidebar, and a comment-event webhook that pushes to Octo IM.
- **Pluggable storage.** PostgreSQL or MySQL for metadata, S3-compatible for
  blobs — both behind narrow interfaces. One static binary, no runtime deps.
- **Horizontally scalable.** Stateless app; run N replicas — per-slug writes
  serialize cluster-wide via database advisory locks.

## Quick start

Bring up the full stack and publish a document:

```bash
git clone https://github.com/Ranwanglc/octo-docs-html && cd octo-docs-html

# App + database + MinIO (+ reverse proxy) — see deploy/ for compose files:
docker compose -f deploy/docker-compose.yml up -d --wait

# Publish a doc over the /v1 API (see docs/AUTH.md for the identity model):
curl -H "Content-Type: application/json" \
  -d '{"slug":"hello","html":"<html><body><h1>Hi</h1></body></html>"}' \
  http://localhost:8080/v1/docs
#   → { "data": { "url": "/d/hello/v/1", "version": 1, ... } }

open http://localhost:8080/d/hello/v/1
```

Standalone (no Octo) works out of the box; wiring it into Octo is a set of config
flips — see [Octo integration](#octo-integration). Going to production:
**[docs/SELF_HOSTING.md](docs/SELF_HOSTING.md)**.

## How it works

| Concept | Detail |
| --- | --- |
| **Document** | `slug` + monotonically increasing `version` → immutable HTML blob |
| **URL** | `/d/<slug>/v/<version>` (`/v/latest` resolves to the newest published version) |
| **Comments** | append-only event log; each version renders a folded snapshot |
| **Artifacts** | every commentable element is stamped `data-odoc-aid="<hash>"` so comments anchor by identity, not DOM position |
| **Auth** | private by default — **author = creator uid**, per-doc share **code** = read + comment, per-uid `doc_grants` = named readers ([docs/AUTH.md](docs/AUTH.md)) |
| **Assets** | inline media served via HMAC-signed sub-resource URLs so native `<img>` loads authorize without a header |
| **Storage** | PostgreSQL **or** MySQL (metadata) + S3-compatible (blobs), behind `MetadataStore` / `BlobStore` interfaces |
| **Scaling** | stateless app; concurrent same-slug writes serialize via database advisory locks |

Dependencies flow one way — **transport → service → storage** — around a
dependency-free `core` kernel. Full design, data model, and the `/v1` API spec are
in **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)**; rationale, threat model, and
backup/upgrade in **[DESIGN.md](DESIGN.md)**.

## Octo integration

Every integration is an independent config flip; unset ⇒ that path is inert and
the server behaves as a standalone deploy.

| Capability | Env var(s) | Effect |
| --- | --- | --- |
| **Login (http provider)** | `OCTO_SERVER_BASE_URL`, `LOGIN_ENABLED` | `/v1/auth/login` verifies a viewer against octo-server and populates the session |
| **Bot-token auth** | `BOT_AUTH_ENABLED` (+ `OCTO_SERVER_BASE_URL`) | accepts an Octo bot token; the bot's owner uid becomes the doc creator/author |
| **Doc-binding channel** | `OCTO_DOC_BINDING_URL`, `OCTO_DOC_BINDING_TTL_MS` | asks octo-server whether a uid may see a slug's binding (per-uid visibility), cached briefly |
| **Web-docs registration** | `DOCS_BACKEND_REGISTER_URL`, `DOCS_BACKEND_REGISTER_TOKEN` | registers each published HTML doc into the docs-backend sidebar |
| **Comment-event webhook** | `OCTO_WEBHOOK_URL`, `OCTO_DOC_EVENT_WEBHOOK_TOKEN` | pushes new-comment events to Octo IM (token required, sent as `X-Octo-Doc-Webhook-Token`) |
| **Superadmin/owner** | `OWNER` | designates which signed-in Login sees the `/me` catalog |

## Agent workflow

octo-docs-html is **API-first**: an agent turns a prompt into a self-contained
HTML document and drives the doc lifecycle over the versioned `/v1` API. Authoring
is **remote-first** — a doc lives on the server from creation as a mutable draft,
and promoting the draft mints an immutable version. The client
(`octo-cli`, packaged separately) wraps these calls, but the API is the contract:

```bash
export BASE=https://docs.example.com

# Save HTML as a private draft, then promote it to an immutable version:
curl -H "Content-Type: application/json" \
  -d '{"slug":"explainer","html":"<html><body><h1>Hi</h1></body></html>"}' \
  "$BASE/v1/docs"                                    # → /d/explainer/v/1

# Mint a per-doc read + comment share code:
curl -X POST "$BASE/v1/docs/explainer/share"
#   → { "data": { "code": "…", "url": ".../d/explainer/v/1?code=***" } }
```

Authenticate as the creator via an Octo session/bot token (see
[Octo integration](#octo-integration) and [docs/AUTH.md](docs/AUTH.md)). See
**[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** for the request lifecycle and the
full `/v1` surface (docs, drafts, comments, reactions, assets, share, grants,
agent element get/replace).

## Configuration

12-factor: every knob is an environment variable (full list in
**[.env.example](.env.example)**). The essentials:

| Variable | Default | Purpose |
| --- | --- | --- |
| `STORAGE_DRIVER` | `postgres` | metadata backend — `postgres` or `mysql` |
| `DATABASE_URL` | _(required)_ | database connection string |
| `S3_BUCKET` / `S3_ENDPOINT` | `octo-doc` / _(AWS)_ | blob store — for MinIO/R2 set the endpoint + `S3_FORCE_PATH_STYLE=1` |
| `ASSET_SIGNING_SECRET` | _(falls back to `WRITE_TOKEN`)_ | HMAC key for signed inline-asset URLs |
| `PG_POOL_MAX` | `10` | max connections **per pool**; the app keeps two (queries + advisory locks) |
| `FRAME_ANCESTORS` | `'none'` | CSP embedding policy for rendered docs; also derives the postMessage sender allowlist |
| `MAX_HTML_BYTES` | `5242880` | per-document size cap (5 MiB) |
| `MAX_ASSET_BYTES` | `26214400` | per-asset size cap (25 MiB) |

Octo-integration variables are listed under [Octo integration](#octo-integration).

The server binary `cmd/octo-doc` exposes these subcommands:

```bash
octo-doc serve       # run the HTTP server (default)
octo-doc migrate     # apply the database schema (idempotent)
octo-doc bootstrap   # mint and print the first write token
octo-doc gc-assets   # delete unreferenced media assets past a grace window
octo-doc health      # local healthcheck (used by the container)
octo-doc version     # print the build version
```

## Development

Go 1.26 · [chi](https://github.com/go-chi/chi) router ·
[pgx](https://github.com/jackc/pgx) · [aws-sdk-go-v2](https://github.com/aws/aws-sdk-go-v2).

```bash
make build        # build bin/octo-doc (server)
make test         # all tests (db/s3 suites skip without OCTO_TEST_* env)
make check        # fmt + vet + lint + test — the local gate
```

The `core` kernel (artifact stamping, the comment event-log fold, anchor
reconciliation) is a **byte-equivalent port** whose observable output is pinned by
self-contained tests; keep `go test ./internal/core/` green. To run the storage
and e2e suites against real services, start the database + MinIO and export the
`OCTO_TEST_*` variables (see the `Makefile` defaults).

See **[CONTRIBUTING.md](CONTRIBUTING.md)** before opening a pull request, and
**[CHANGELOG.md](CHANGELOG.md)** for release notes. All participation is governed
by our **[Code of Conduct](CODE_OF_CONDUCT.md)**.

## Security

Please report vulnerabilities privately — see the **[Security Policy](SECURITY.md)**.
Do not open a public issue for security reports. Operator hardening guidance is in
the [production checklist](docs/SELF_HOSTING.md), and the access-control model is
documented in [docs/AUTH.md](docs/AUTH.md).

## Credits

octo-docs-html builds on the self-hosted `octo-doc` project — itself a
reimplementation of Serena Keyitan's
[tdoc](https://github.com/serenakeyitan/tdoc), and upstream of that, Jesse
Pollak's *bdocs* concept — and adds first-class Octo platform integration. It
keeps the product identical and makes it something you run yourself.

## License

Released under the [MIT License](LICENSE).
