# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Removed

- The `octo` client CLI has been extracted to a separate `octo-cli` project; this
  repo is now API-only. Author against a running server with the `/v1` HTTP API
  (see the README Quick start and [docs/SELF_HOSTING.md](docs/SELF_HOSTING.md)).
- The bundled agent skill's separate repository has been deprecated and archived;
  a replacement will target the new `octo-cli`.

### Added

- Server release automation: pushing a `v*` tag cross-compiles `octo-doc` for
  linux/macOS (amd64/arm64) and attaches the binaries + `SHA256SUMS` to a GitHub
  Release. Binaries are version-stamped — `octo-doc version` prints the build.

## [0.3.0] - 2026-07-07

### Changed

- **Rebranded every octo-doc-owned `tdoc` identifier to `odoc`**: the aid
  attribute is now `data-odoc-aid`, overlay CSS classes are `odoc-*`, the boot
  global is `window.__ODOC__`, the agent login is `odoc-agent`, and the artifact
  opt-in marker is `data-odoc-artifact`. The aid **hash** is unchanged (computed
  over stripped content), so golden parity holds.
- Removed all Cloudflare/Workers framing from docs and comments — octo-doc is
  described on its own terms (self-hosted, PostgreSQL + S3).
- Repository home is `github.com/Mininglamp-OSS/octo-docs-html`: the Go module path, imports,
  `octo update` release repo, `REPO_URL` default, and the ghcr image
  (`ghcr.io/mininglamp-oss/octo-docs-html`) all point there. (Historical: `go install
  github.com/Mininglamp-OSS/octo-docs-html/cmd/octo@latest` installed the client CLI at the
  time of this release; the `octo` CLI has since been removed from this repo —
  see the Unreleased section.)

### Fixed

- A fresh `?code=` or Bearer credential now takes precedence over a stale
  capability cookie, so rotating a share code cuts off old links while a
  recipient's new link keeps working, and `octo new --open` reaches the author's
  draft even with a pre-existing reader cookie.
- The Share (mint-code) button is hidden from readers — it is an author-only
  action and previously 404'd when a reader clicked it.
- `promote` is idempotent past the version commit: a failed draft-clear no longer
  surfaces as an error that a retry would turn into a duplicate version.

### Removed

- The legacy `TDOC_*` env fallback and `~/.tdoc` / `~/tdocs` paths — `OCTO_*` is
  the only configuration surface.
- `docs/MIGRATING_FROM_WORKERS.md` (Cloudflare migration guide).

### Breaking

- Documents published before this release carry `data-tdoc-aid` in their stored
  HTML; their comment anchors will not resolve until the doc is re-published (the
  overlay now looks for `data-odoc-aid`).

### Changed

- **Rewrote the server in Go** (was TypeScript/Node). The domain kernel
  (`internal/core`) is a byte-equivalent port verified against golden fixtures;
  see [docs/PORTING.md](docs/PORTING.md). The server is now a single static
  binary with no runtime dependencies.
- **Unified storage on PostgreSQL + S3-compatible object storage.** The previous
  pluggable `sqlite+fs` / `postgres+s3` selection was removed; PostgreSQL (via
  pgx) and an S3-compatible store (via aws-sdk-go-v2) are now required.
- `overlay.js` is embedded into the binary with `go:embed` (single source of
  truth, no runtime path lookup).
- The Docker image is now a distroless static build (rootless).

### Removed

- The TypeScript implementation, Node tooling (pnpm, tsup, vitest, eslint), and
  the bundled `/tdoc` agent skill (extracted to a separate repository).

### Added

- `make` targets for the full local quality gate; `golangci-lint` configuration.
- A reusable storage contract suite run against the in-memory, PostgreSQL, and S3
  implementations, plus a full-lifecycle e2e test against live PostgreSQL + MinIO.
- `octo-doc health` subcommand for container healthchecks.
