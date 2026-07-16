# Self-hosting octo-doc

From nothing to a live, TLS-secured doc server in ~15 minutes on a $5 VPS.

## TL;DR

```bash
git clone https://github.com/lml2468/octo-doc && cd octo-doc
DOMAIN=docs.example.com docker compose -f deploy/docker-compose.yml up -d --wait
TOKEN=$(curl -sX POST http://localhost:8080/v1/admin/bootstrap | jq -r .data.token)
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
git clone https://github.com/lml2468/octo-doc
cd octo-doc
# DOMAIN drives Caddy's automatic Let's Encrypt cert.
DOMAIN=docs.example.com docker compose -f deploy/docker-compose.yml up -d --wait
```

That's it. One compose file brings up the whole stack: the octo-doc app,
PostgreSQL, MinIO (S3-compatible blob storage), and Caddy (auto-TLS). `--wait`
blocks until the app healthcheck passes (typically a few seconds; the whole `up`
is well under 2 minutes on a clean Ubuntu 24.04 box). Schema migrations run
automatically on app start (`octo-doc migrate` is also exposed for manual runs).

### 4. Mint a write token

```bash
TOKEN=$(curl -sX POST http://localhost:8080/v1/admin/bootstrap | jq -r .data.token)
echo "$TOKEN"          # save this — bootstrap only works once
```

(Equivalently, run `octo-doc bootstrap` inside the app container —
`docker compose -f deploy/docker-compose.yml exec app octo-doc bootstrap` —
which mints and prints the same first token.)

Prefer a fixed token you control? Set `WRITE_TOKEN=...` in a `.env` file next to
the compose file before `up` (bootstrap is then disabled).

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
still need a PostgreSQL instance and an S3-compatible bucket reachable from it
(a managed Postgres + an S3/MinIO/R2-compatible bucket, or self-hosted).

Grab a prebuilt binary for linux/macOS (amd64/arm64) from the
[latest release](https://github.com/lml2468/octo-doc/releases/latest) — each is
listed in `SHA256SUMS` — or build from source:

```bash
make build                    # or: go build -o octo-doc ./cmd/octo-doc

export DATABASE_URL="postgres://octo:octo@localhost:5432/octodoc"
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
Environment=DATABASE_URL=postgres://octo:octo@localhost:5432/octodoc
Environment=S3_BUCKET=octo-doc S3_ENDPOINT=http://localhost:9000 S3_REGION=us-east-1
Environment=S3_FORCE_PATH_STYLE=true S3_ACCESS_KEY_ID=... S3_SECRET_ACCESS_KEY=...
Restart=always
User=octo
[Install]
WantedBy=multi-user.target
```

---

## Storage configuration (Postgres + S3)

octo-doc always stores metadata in PostgreSQL and blobs in an S3-compatible
bucket — these are the two required backends, configured purely by env. The
default compose file wires up bundled Postgres + MinIO automatically; point
these vars at managed services to use your own:

```bash
DATABASE_URL=postgres://octo:octo@postgres:5432/octodoc

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
- [ ] **Backups** — `pg_dump` the metadata + S3 versioning/lifecycle (or
      `aws s3 sync`) for the blobs. See [DESIGN.md](./DESIGN.md#backup--restore).
- [ ] **Rate limits** — tune `RATE_LIMIT_MAX` / `RATE_LIMIT_WINDOW_MS` for your
      audience.

All knobs are documented in [`.env.example`](../.env.example).

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
