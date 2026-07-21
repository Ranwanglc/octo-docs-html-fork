# octo-doc skill

The agent authoring skill for [octo-doc](https://github.com/Mininglamp-OSS/octo-docs-html) — an
API-first skill that turns a prompt into a self-contained interactive HTML
document, publishes it to your self-hosted octo-doc server over the `/v1` HTTP
API, shares it with text- and artifact-anchored inline commenting, and iterates it
from those comments.

octo-doc is an **API-only** server (Go + PostgreSQL/MySQL + S3-compatible storage). This
skill drives that API — there is no compiled client binary. `scripts/octo.sh`
(bash + curl + jq) is a thin convenience wrapper over the endpoints; the raw
contract is in `references/api.md`.

## Layout

```
SKILL.md              the skill definition (workflow, triggers, config)
scripts/octo.sh       curl+jq wrapper over the /v1 API (bootstrap, publish,
                      draft, promote, share, pull, comment, reply, react, assets)
references/api.md         full /v1 endpoint reference (methods, auth, bodies)
references/authoring.md   HTML generation rules + the default-styling contract
references/anchoring.md   comment anchor JSON shapes and how to interpret them
templates/doc.html        the starting HTML skeleton for a new doc
```

## Requirements

- A running octo-doc server (a hosted instance, or the local Docker stack — see
  the octo-doc repo's `docs/SELF_HOSTING.md`).
- `bash`, `curl`, and `jq` on PATH (for `scripts/octo.sh`).

## Usage

```bash
export OCTO_BASE_URL="https://docs.example.com"
export OCTO_TOKEN="$(scripts/octo.sh bootstrap)"   # first run on a fresh server

# author → save a draft → publish → share
scripts/octo.sh draft   my-explainer ./doc.html "Compound interest, explained"
scripts/octo.sh promote my-explainer               # → /d/my-explainer/v/1
scripts/octo.sh share   my-explainer               # → a read + comment ?code= link
```

See [SKILL.md](SKILL.md) for the full Create / Edit / Share workflow and
`scripts/octo.sh help` for every command. Config resolves from `OCTO_BASE_URL`,
`OCTO_TOKEN` (author), and `OCTO_CODE` (reader).
