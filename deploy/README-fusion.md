# octo-doc — Fusion (OCT-130) deployment guide

> Companion to the base `deploy/README.md`. Use this file when octo-doc is being
> deployed as a fusion member service (i.e. plugged into the octo identity
> bridge from OCT-135). For a stand-alone doc host, follow the base README.

This overlay adds three fusion-specific env vars, flips `ALLOW_BOOTSTRAP` to a
production-safe default, and splits PostgreSQL into a dedicated `octodoc-pg`
instance so it can coexist with other octo services on the same host.

## Deployment modes — pick one

Three compose paths ship in `deploy/`, layered on the base
`docker-compose.yml`:

1. **Stand-alone PostgreSQL** (base only) — `docker compose -f
   deploy/docker-compose.yml up -d`. Self-contained app + Caddy + PG + MinIO.
2. **Fusion PostgreSQL** (this guide) — base + `docker-compose.fusion.yml`.
   Dedicated `octodoc-pg`, fusion identity env, production-safe bootstrap.
3. **Self-contained MySQL** — base + `docker-compose.mysql.yml`. Swaps the
   metadata store to a **bundled MySQL 8** container (no pre-existing octo
   MySQL required) and injects the full env contract
   (`STORAGE_DRIVER=mysql`, identity, asset signing, docs-backend
   registration, doc_binding). One command brings a brand-new environment up:
   ```bash
   cp deploy/.env.example deploy/.env
   # edit deploy/.env — fill MYSQL_* + secrets (+ OCTO_SERVER_BASE_URL / DOCS_BACKEND_* if used)
   docker compose --env-file deploy/.env \
     -f deploy/docker-compose.yml \
     -f deploy/docker-compose.mysql.yml up -d --build
   ```
   `--build` compiles the app image from this repo's `deploy/Dockerfile` (the
   source supports MySQL). The compose `image:` defaults to a local tag
   `octo-docs-html:local`; once a MySQL-capable image is published, set
   `APP_IMAGE=<image>` in the env file to pull it instead of building.
   Env-key reference: `deploy/.env.example`. The rest of *this* file documents
   the PostgreSQL fusion path (mode 2).

## Deploy prerequisites / required environment variables

Every variable below must be set (or explicitly left at the noted default)
before the service is exposed to real users. Silent-failure knobs are called
out — those are the ones that boot fine but quietly break the fusion contract.

| Name                    | Default in fusion overlay | Effect if unset / wrong                                                                                                                    |
| ----------------------- | ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `LOGIN_ENABLED`         | `false`                   | Overlay UI toggle: `true` in fusion (docs_proxy is in front, overlay shows the identity chip), `false` for stand-alone (overlay stays anonymous). Since OCT-145 方案 C this only affects the UI — the trust-header identity middleware is always on. |
| `FRAME_ANCESTORS`       | `'none'`                  | CSP frame-ancestors. Must include the octo-web panel origin, otherwise the panel iframe is blocked with a CSP error.                       |
| `OWNER`                 | *(empty)*                 | Extra octo logins to promote to CapAuthor. Empty is intentional per OCT-130 (reuse octo superAdmin via role). Set only for extras.         |
| `ALLOW_BOOTSTRAP`       | `false`                   | `POST /v1/admin/bootstrap` first-token mint. **Must stay false in production** — open bootstrap on the internet is a token-grab race.       |
| `WRITE_TOKEN`           | *(empty)*                 | If set, THE Bearer token for every write endpoint. Prefer this over bootstrap once initial setup is done.                                  |
| `DATABASE_URL`          | wired to `octodoc-pg` w/ `sslmode=disable` | PG connection string. Overlay 已用 `${PG_USER:-octodoc}`/`${PG_PASSWORD:-octodoc}`/`${PG_DB:-octodoc_fusion}` 拼好并加 `sslmode=disable`（容器内网无 TLS，pgx 默认 prefer 会静默降级）。外接 PG 时整个覆盖。 |
| `PG_USER` / `PG_PASSWORD` / `PG_DB` | `octo` / `octo` / `octodoc_fusion` | Credentials for `octodoc-pg`. Change the password before staging.                                                       |
| `S3_BUCKET` / `S3_ENDPOINT` / `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` | `octo-doc` / MinIO / `minioadmin` / `minioadmin` | Object storage. Reuse octo's MinIO or a dedicated bucket. If `S3_ENDPOINT` is set, the two credentials become mandatory. |
| `BASE_URL`              | *(empty)*                 | Public origin used to build absolute `/d/<slug>/v/<n>` links in API responses. Set to the real https URL in prod, or links go relative.    |
| `COOKIE_SECURE`         | `true`                    | Leave true behind TLS; only flip to false for plain-HTTP local dev.                                                                        |

The compose overlay reads these from `deploy/.env.fusion` (copy from
`deploy/.env.example`). The systemd unit reads them from
`/etc/octo-doc/octo-doc.env` (copy from `deploy/systemd/octo-doc.env.example`).

## Option A — docker-compose (recommended)

```bash
cp deploy/.env.example deploy/.env.fusion
# edit deploy/.env.fusion — fill LOGIN_ENABLED / FRAME_ANCESTORS / OWNER / secrets

docker compose \
  --env-file deploy/.env.fusion \
  -f deploy/docker-compose.yml \
  -f deploy/docker-compose.fusion.yml \
  up -d
```

The overlay:

- adds fusion env (`LOGIN_ENABLED`, `OWNER`); `OCTO_OPENAPI_URL` / `OCTO_USERINFO_URL` are removed (OCT-145 方案 C — doc reads identity from reverse-proxy trust headers, no userinfo call);
- flips `ALLOW_BOOTSTRAP` default to `false`;
- introduces `octodoc-pg` (name / DB / volume / host port 5433) and disables
  the base `postgres` service via the `disabled` profile;
- adds a `migrate` one-shot service (profile `migrate`) that runs
  `octo-doc migrate` against `octodoc-pg` and exits.
- rebinds the app container's `8080` publish to `127.0.0.1:8080` — all
  external traffic must go through Caddy (so CSP/TLS/FRAME_ANCESTORS actually
  apply). Access `curl http://127.0.0.1:8080/healthz` on the host, or hit
  `https://<DOMAIN>/healthz` via Caddy from outside.

### Apply migrations

```bash
docker compose \
  --env-file deploy/.env.fusion \
  -f deploy/docker-compose.yml \
  -f deploy/docker-compose.fusion.yml \
  --profile migrate run --rm migrate
```

### Bootstrap the first write token (one-shot)

Bootstrap is off by default. To mint the initial token:

```bash
# 1. Temporarily flip in deploy/.env.fusion:  ALLOW_BOOTSTRAP=true
docker compose --env-file deploy/.env.fusion -f deploy/docker-compose.yml -f deploy/docker-compose.fusion.yml up -d app

# 2. Mint the token (POST — returns 409 after the first successful call):
curl -sS -X POST http://127.0.0.1:8080/v1/admin/bootstrap

# 3. Store the token as WRITE_TOKEN, flip ALLOW_BOOTSTRAP back to false, redeploy:
docker compose --env-file deploy/.env.fusion -f deploy/docker-compose.yml -f deploy/docker-compose.fusion.yml up -d app
```

## Option B — systemd (alternative)

For hosts that manage PG / MinIO out-of-band (external services or existing
systemd units), install octo-doc as a native systemd service. Full install
steps are in the header comment of `deploy/systemd/octo-doc.service`.

Short version:

```bash
sudo useradd -r -s /usr/sbin/nologin -d /var/lib/octo-doc octodoc
sudo install -o octodoc -g octodoc /path/to/octo-doc /usr/local/bin/octo-doc
sudo install -o octodoc -g octodoc -d /var/lib/octo-doc /etc/octo-doc /var/log/octo-doc
sudo install -o root -g octodoc -m 0640 deploy/systemd/octo-doc.env.example /etc/octo-doc/octo-doc.env
# edit /etc/octo-doc/octo-doc.env
sudo -u octodoc /usr/local/bin/octo-doc migrate
sudo install -o root -g root -m 0644 deploy/systemd/octo-doc.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now octo-doc.service
```

## Acceptance checks (record output in the PR)

1. Healthz — `curl -sS -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/healthz` → `200`.
2. Migrate — `octo-doc migrate` exits `0` and creates the schema in `octodoc_fusion`.
3. Bootstrap — mint a token via `POST /v1/admin/bootstrap`, then read a doc using
   that token as `Authorization: Bearer <token>`; expect `200` (or `404` for a
   slug that doesn't exist yet — the point is that auth accepted the token).

## Local build acceleration (developer-only, NOT committed)

Behind GFW `go mod download` times out on `proxy.golang.org`. For local builds
only, an alternative Dockerfile with `ENV GOPROXY=https://goproxy.cn,direct`
lives at `deploy/Dockerfile.local`, plus a matching compose override at
`deploy/docker-compose.fusion.local.yml`. Both are excluded via `.gitignore` and
`.git/info/exclude`, so they cannot be committed accidentally. Do not add a
GOPROXY to the shipped Dockerfile — upstream builds must stay proxy-neutral.

Example local invocation:

```bash
docker compose \
  --env-file deploy/.env.fusion \
  -f deploy/docker-compose.yml \
  -f deploy/docker-compose.fusion.yml \
  -f deploy/docker-compose.fusion.local.yml \
  build app
```
