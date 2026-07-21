# Self-hosting octo-doc

From nothing to a live, TLS-secured doc server in ~15 minutes on a $5 VPS.

## TL;DR

```bash
git clone https://github.com/Mininglamp-OSS/octo-docs-html && cd octo-docs-html
cp deploy/.env.example deploy/.env    # then fill in DB password, S3 keys, ASSET_SIGNING_SECRET, etc.
# The overlay disables bootstrap by default: either set WRITE_TOKEN in deploy/.env
# (recommended), or ALLOW_BOOTSTRAP=true for the bootstrap call below (see §4).
# MySQL + MinIO reference stack (base compose defaults to PostgreSQL; the overlay swaps in MySQL):
DOMAIN=docs.example.com docker compose --env-file deploy/.env \
  -f deploy/docker-compose.yml -f deploy/docker-compose.mysql.yml up -d --wait
TOKEN=$(curl -sX POST http://localhost:8080/v1/admin/bootstrap | jq -r .data.token)   # needs ALLOW_BOOTSTRAP=true
# Publish a doc:
curl -sX POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"slug":"my-doc","title":"My doc","html":"<h1>hi</h1>"}' \
  https://docs.example.com/v1/docs
```

---

## Option A — Docker Compose (recommended)

### 1. Get a server and a domain

Any cheap VPS works (Hetzner CX22, DigitalOcean, Vultr, Lightsail — ~$5/mo).
Point an A record for `docs.example.com` at its IP. Open ports **80** and
**443** in the firewall (Caddy needs both for ACME + serving).

### 2. Install Docker

```bash
curl -fsSL https://get.docker.com | sh
```

### 3. Clone and launch

```bash
git clone https://github.com/Mininglamp-OSS/octo-docs-html
cd octo-docs-html
# The base compose file defaults to PostgreSQL. To run the reference MySQL + MinIO
# stack, layer the MySQL overlay on top (it swaps Postgres out for MySQL) and pass
# the env file so credentials reach the containers.
# deploy/.env is gitignored, so create it from the tracked example first, then fill
# in the required secrets (DB password, S3 keys, ASSET_SIGNING_SECRET — see
# "Required env" below and the annotations in deploy/.env.example).
cp deploy/.env.example deploy/.env
# DOMAIN drives Caddy's automatic Let's Encrypt cert.
DOMAIN=docs.example.com docker compose \
  --env-file deploy/.env \
  -f deploy/docker-compose.yml \
  -f deploy/docker-compose.mysql.yml \
  up -d --wait
```

That's it. This brings up the whole stack: the octo-doc app, MySQL, MinIO
(S3-compatible blob storage), and Caddy (auto-TLS). The MySQL overlay sets
`STORAGE_DRIVER=mysql`, points `DATABASE_URL` at the bundled `mysql` service, and
disables the base Postgres service. `--wait`
blocks until the app healthcheck passes (typically a few seconds; the whole `up`
is well under 2 minutes on a clean Ubuntu 24.04 box). Schema migrations run
automatically on app start (`octo-doc migrate` is also exposed for manual runs).

> **Default stack is PostgreSQL.** Omitting `-f deploy/docker-compose.mysql.yml`
> launches the base stack, which runs PostgreSQL instead of MySQL.

### 4. Mint a write token

The MySQL overlay ships with `ALLOW_BOOTSTRAP=false` (see `deploy/.env.example`),
so the one-shot bootstrap endpoint is disabled by default. Pick one:

**Option 1 (recommended) — set a fixed `WRITE_TOKEN`.** Put `WRITE_TOKEN=<your-token>`
in `deploy/.env` before `up`; that value is your write credential (bootstrap stays
disabled). This is the intended path for the overlay.

**Option 2 — enable bootstrap once.** Set `ALLOW_BOOTSTRAP=true` in `deploy/.env`
before `up`, then mint the first token:

```bash
TOKEN=$(curl -sX POST http://localhost:8080/v1/admin/bootstrap | jq -r .data.token)
echo "$TOKEN"          # save this — bootstrap only works once
```

Equivalently, run `octo-doc bootstrap` inside the app container (use the SAME
compose files the stack was launched with, or `exec` targets the wrong stack):

```bash
docker compose --env-file deploy/.env \
  -f deploy/docker-compose.yml -f deploy/docker-compose.mysql.yml \
  exec app octo-doc bootstrap
```

After minting, set `ALLOW_BOOTSTRAP=false` again for production.

### 5. Publish from your machine

```bash
BASE=https://docs.example.com
TOKEN="<the token>"
curl -sX POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"slug":"my-doc","title":"My doc","html":"<h1>hi</h1>"}' \
  "$BASE/v1/docs"
# → { "data": { "slug": "my-doc", "version": 1, "url": "/d/my-doc/v/1", ... } }
```

### Verify

```bash
curl -sf https://docs.example.com/v1/ping        # {"data":{"ok":true,"service":"octo-doc"}}
curl -sf https://docs.example.com/d/my-doc/v/1 | grep -q '<h1' && echo OK
```

---

## Option B — The binary (no Docker)

octo-doc compiles to a single static binary with no runtime dependencies. You
still need a MySQL instance and an S3-compatible bucket reachable from it
(a managed MySQL + an S3/MinIO/R2-compatible bucket, or self-hosted).

Grab a prebuilt binary for linux/macOS (amd64/arm64) from the
[latest release](https://github.com/Mininglamp-OSS/octo-docs-html/releases/latest) — each is
listed in `SHA256SUMS` — or build from source:

```bash
make build                    # or: go build -o octo-doc ./cmd/octo-doc

export STORAGE_DRIVER=mysql                   # required; defaults to postgres if unset
export DATABASE_URL="octo:octo@tcp(localhost:3306)/octodoc?charset=utf8mb4&parseTime=true&loc=Local"
export S3_BUCKET=octo-doc
export S3_ENDPOINT=http://localhost:9000     # omit for real AWS S3
export S3_REGION=us-east-1
export S3_FORCE_PATH_STYLE=true              # true for MinIO; false for AWS S3
export S3_ACCESS_KEY_ID=minioadmin
export S3_SECRET_ACCESS_KEY=minioadmin

./octo-doc migrate            # create schema (idempotent)
./octo-doc serve              # listens on :8080
# in another shell — mint the first token:
./octo-doc bootstrap          # or: curl -sX POST localhost:8080/v1/admin/bootstrap | jq -r .data.token
```

Put it behind your own nginx/Caddy/Traefik for TLS — reference configs are in
[`deploy/`](../deploy/) (`nginx.conf.example`, `Caddyfile`,
`traefik.labels.example.yml`).

Run as a systemd service:

```ini
# /etc/systemd/system/octo-doc.service
[Service]
ExecStart=/usr/local/bin/octo-doc serve
Environment=PORT=8080 WRITE_TOKEN=... COOKIE_SECURE=true
Environment=STORAGE_DRIVER=mysql
Environment=DATABASE_URL=octo:octo@tcp(localhost:3306)/octodoc?charset=utf8mb4&parseTime=true&loc=Local
Environment=S3_BUCKET=octo-doc S3_ENDPOINT=http://localhost:9000 S3_REGION=us-east-1
Environment=S3_FORCE_PATH_STYLE=true S3_ACCESS_KEY_ID=... S3_SECRET_ACCESS_KEY=...
Restart=always
User=octo
[Install]
WantedBy=multi-user.target
```

---

## Storage configuration (MySQL + S3)

octo-doc stores metadata in a SQL database and blobs in an S3-compatible bucket —
these are the two required backends, configured purely by env. The metadata store
is pluggable: `STORAGE_DRIVER` selects `postgres` (the code default) or `mysql`.
The reference MySQL stack (base compose + `docker-compose.mysql.yml` overlay)
wires up a bundled MySQL + MinIO automatically; point these vars at managed
services to use your own:

```bash
STORAGE_DRIVER=mysql               # required for MySQL; omit/unset falls back to postgres
DATABASE_URL=octo:octo@tcp(mysql:3306)/octodoc?charset=utf8mb4&parseTime=true&loc=Local

S3_BUCKET=octo-doc
S3_ENDPOINT=http://minio:9000      # omit for real AWS S3
S3_REGION=us-east-1
S3_FORCE_PATH_STYLE=true           # true for MinIO; false for AWS S3
S3_ACCESS_KEY_ID=minioadmin
S3_SECRET_ACCESS_KEY=minioadmin
```

Schema creation is idempotent — `octo-doc migrate` (run automatically at app
start) is safe to re-run.

---

## Production hardening checklist

- [ ] **Separate doc origin.** Serve docs from `d.example.com`, distinct from any
      trusted panel/app origin, so untrusted inline JS can't read app cookies.
      See the two-site block in [`deploy/Caddyfile`](../deploy/Caddyfile).
- [ ] **`COOKIE_SECURE=true`** (default) — cookies only over HTTPS.
- [ ] **`FRAME_ANCESTORS`** — keep `'none'` unless you intentionally embed docs.
- [ ] **Set `WRITE_TOKEN`** explicitly and `ALLOW_BOOTSTRAP=false` once set up.
- [ ] **Docs are private by default** — access is per-document via share codes
      (mint one with `POST /v1/docs/<slug>/share`, or the Share button), not a
      global flag. See [AUTH.md](./AUTH.md).
- [ ] **Backups** — `mysqldump` the metadata + S3 versioning/lifecycle (or
      `aws s3 sync`) for the blobs. See [DESIGN.md](./DESIGN.md#backup--restore).
- [ ] **Rate limits** — tune `RATE_LIMIT_MAX` / `RATE_LIMIT_WINDOW_MS` for your
      audience.

All knobs are documented in [`.env.example`](../.env.example) (MySQL reference
values). For the full MySQL service wiring — bundled MySQL + MinIO, disabled
Postgres, and exactly which env keys are passed through — see
[`deploy/docker-compose.mysql.yml`](../deploy/docker-compose.mysql.yml).

### Required env (must be set)

The code defaults are PostgreSQL + most features off, so for the MySQL + iframe +
sidebar-registration deploy these MUST be set explicitly (each is annotated `[必配]`
in `.env.example`):

- `STORAGE_DRIVER=mysql` — code default is `postgres`; unset connects the wrong DB.
- `DATABASE_URL` (or bundled-MySQL `MYSQL_*`) — the MySQL DSN. **Must point at the
  SAME MySQL database docs-backend uses** (see the shared-database note below).
- `S3_ENDPOINT` / `S3_BUCKET` / `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` — blob store.
- `S3_PREFIX` — object-key namespace, e.g. `octo-docs-html/test`.
- `S3_FORCE_PATH_STYLE=1` — required for MinIO bucket addressing.
- `LOGIN_ENABLED=true` — identity-bridge master switch; unset = login silently falls back.
- `OCTO_SERVER_BASE_URL` — octo-server origin doc calls for `verify(-bot)`; unset = no identity.
- `BOT_AUTH_ENABLED=true` — bot-token auth; unset = bot write endpoints can't authenticate.
- `FRAME_ANCESTORS` — CSP frame-ancestors; default `'none'` blocks the panel iframe (blank page).
- `ASSET_SIGNING_SECRET` — signs inline `<img>` URLs; missing = images 404.

> **Shared database with docs-backend (critical).** octo-docs-html and
> docs-backend must use the **same MySQL database**. `doc_meta` and `doc_member`
> are shared tables: when a reader is granted access, octo-docs-html writes the
> `doc_member` row **directly via SQL** in its own `DATABASE_URL` connection (the
> grant "mirror" — not an HTTP call), and docs-backend's access preflight reads
> that same table. So `DATABASE_URL` here must resolve to the exact database
> docs-backend runs against (same host/port/db name). Giving octo-docs-html its
> own separate database silently breaks reader access: the mirrored `doc_member`
> rows land in the wrong DB, docs-backend never sees them, and forwarded/shared
> docs 404 for readers even though the grant "succeeded". (The mirror only runs
> when `STORAGE_DRIVER=mysql`; under postgres it is a no-op.)

---

## Troubleshooting

- **`docker compose up` hangs on `--wait`** → check `docker compose logs app`.
  Usually a bad `DATABASE_URL`, an unreachable `S3_ENDPOINT`, or a taken port 8080.
- **Caddy can't get a cert** → DNS A record not pointing at the box yet, or
  ports 80/443 blocked. `docker compose logs caddy` shows the ACME error.
- **`bootstrap` returns 409** → a token already exists (or `WRITE_TOKEN` is
  set). That's expected — bootstrap is one-shot. Use the token you saved, or
  set `WRITE_TOKEN`.
- **Publish 413** → doc exceeds `MAX_HTML_BYTES` (5 MiB default).
