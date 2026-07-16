# octo-doc demo

A self-contained showcase of everything octo-doc does. The demo document is
*about* octo-doc and is *served by* octo-doc — you review the product inside the
product. The three `index.v*.html` files are ready-to-publish authoring sources;
you drive the flow yourself against the `/v1` HTTP API, exactly as an agent would.

It exercises the full surface:

- **Interactive HTML artifacts** — a live SVG adoption chart with a
  Monthly/Cumulative toggle (vanilla JS, no dependencies).
- **Remote-first drafts → immutable versions** — `PUT /v1/docs/{slug}/draft`
  saves a mutable draft; `POST /v1/docs/{slug}/draft/promote` freezes it as
  **v1**; iterate and promote again for **v2**, **v3**.
- **Default-private + share codes** — a fresh doc is author-only;
  `POST /v1/docs/{slug}/share` mints a read+comment `?code=` link anyone can open.
- **Anchored comments that re-anchor across versions**, threaded replies, and
  **agent verdicts** (`applied` / `partial` / `question`), plus emoji reactions.
- **Comment-driven editing** — v3 adds a section that answers a reviewer's
  "does it survive a full rewrite?" question, the review loop in action.

## Prerequisites

A running octo-doc server. The simplest path is the local stack:

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.local.yml up -d --build --wait

BASE=http://localhost:18080
TOKEN=local-test-token          # the local stack's write token = author
```

The app serves at **http://localhost:18080** (see `docs/SELF_HOSTING.md` for
production, and `docs/AUTH.md` for the capability/code model).

> The raw `curl` calls below are what a packaged client (`octo-cli`) wraps. This
> repo is API-only; every authoring action is a `/v1` request.

## Walkthrough

```bash
D=examples/demo
AUTH=(-H "Authorization: Bearer $TOKEN")
JSON=(-H "Content-Type: application/json")

# 1) Publish the doc directly (saves + creates v1). POST /v1/docs takes the HTML
#    inline; jq builds the JSON body from the file.
jq -n --arg slug octo-demo \
      --arg title "octo-doc — documents you can talk to" \
      --rawfile html $D/index.v1.html \
      '{slug:$slug, title:$title, html:$html}' \
  | curl -s "${AUTH[@]}" "${JSON[@]}" -d @- "$BASE/v1/docs"      # → /d/octo-demo/v/1

# 2) Iterate: chart projection + a Versioning section. Save a draft, then promote → v2.
jq -n --rawfile html $D/index.v2.html '{html:$html}' \
  | curl -s "${AUTH[@]}" "${JSON[@]}" -X PUT -d @- "$BASE/v1/docs/octo-demo/draft"
curl -s "${AUTH[@]}" -X POST "$BASE/v1/docs/octo-demo/draft/promote"   # → /d/octo-demo/v/2

# 3) Iterate again: a section answering the anchoring question → v3.
jq -n --rawfile html $D/index.v3.html '{html:$html}' \
  | curl -s "${AUTH[@]}" "${JSON[@]}" -X PUT -d @- "$BASE/v1/docs/octo-demo/draft"
curl -s "${AUTH[@]}" -X POST "$BASE/v1/docs/octo-demo/draft/promote"   # → /d/octo-demo/v/3

# 4) Share it — mint a read+comment link (the doc is private by default).
curl -s "${AUTH[@]}" -X POST "$BASE/v1/docs/octo-demo/share"          # → { data: { code, url: ".../d/octo-demo/v/3?code=<code>" } }
```

Open the printed `?code=` link in a browser: the code is exchanged for a cookie,
the URL is cleaned, and the doc renders with the review overlay. Select any
sentence to leave an anchored comment; open the version picker to compare v1/v2/v3.

To seed the review threads shown in the screenshots (a human comment + an agent
`applied` reply + a 👍), post through the `/v1` API. The reader comment uses the
share code as its credential; the author reply uses the write token.

Anchored comments are normally created in the browser overlay — selecting text
computes the structured `anchor` object (aid + fingerprint + fallback) for you.
Over the API you can either send that object or omit `anchor` for a doc-level
comment, as below:

```bash
CODE=$(curl -s "${AUTH[@]}" -X POST "$BASE/v1/docs/octo-demo/share" | jq -r .data.code)

# A reader comment (code = read+comment capability).
CID=$(curl -s -H "Authorization: Bearer $CODE" "${JSON[@]}" \
  -d '{"slug":"octo-demo","version":3,
       "text":"Does the guarantee survive a full rewrite of the paragraph?"}' \
  "$BASE/v1/comments" | jq -r .data.id)

# The author resolves it (write token) with a verdict.
curl -s "${AUTH[@]}" "${JSON[@]}" \
  -d "{\"slug\":\"octo-demo\",\"parent_id\":\"$CID\",\"status\":\"applied\",\"applied_in\":3,
       \"text\":\"Yes — v3's new \\\"What happens when the text is rewritten?\\\" section covers it.\"}" \
  "$BASE/v1/agent/replies"

# React with a 👍 (reader capability).
curl -s -H "Authorization: Bearer $CODE" "${JSON[@]}" \
  -d "{\"slug\":\"octo-demo\",\"comment_id\":\"$CID\",\"emoji\":\"👍\",\"version\":3}" \
  "$BASE/v1/reactions"
```

> Publishing is **immutable and append-only** — each promote freezes the current
> draft as the next version. To start over, `curl "${AUTH[@]}" -X DELETE
> "$BASE/v1/docs/octo-demo"` and re-run.

## Files

| File            | What it is                                             |
|-----------------|--------------------------------------------------------|
| `index.v1.html` | The interactive self-intro document (version 1)        |
| `index.v2.html` | Revised (v2: chart projection + a Versioning section)  |
| `index.v3.html` | Revised (v3: anchoring-states section answering the review question) |

All three HTML files are fully self-contained (inline CSS + JS, no external
assets), so they render offline and stay within the overlay's content-security
policy.
