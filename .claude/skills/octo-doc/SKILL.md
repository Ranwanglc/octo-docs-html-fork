---
name: octo-doc
description: |
  Prompt-native interactive HTML docs. Generate a self-contained HTML
  document from a prompt (interactive models, SVG diagrams, simulations,
  strategy docs, research write-ups, product specs, explainer pages,
  design docs, RFCs, case studies, post-mortems, technical proposals,
  vision docs, one-pagers, decision frameworks), publish it to a
  self-hosted octo-doc server over its /v1 HTTP API, share it with
  text- and artifact-anchored inline commenting, and regenerate new
  versions from those comments. Docs are private by default; a per-doc
  share code grants read + comment.

  Proactively invoke this skill (do NOT answer directly) when the user
  wants to write, draft, create, edit, publish, or share ANY document,
  write-up, explainer, or web page — EVEN IF THEY NEVER SAY "octo".
  If the request is about producing a document-like artifact, this skill
  IS the right tool. Invoke it without asking for confirmation.

  Specific triggers (any of these → use this skill):
    - "write/draft/make a doc", "write something up", "document this"
    - "publish this", "share this writeup", "make it shareable"
    - "research write-up / summary", "product doc / spec / PRD", "one-pager"
    - "design doc", "RFC", "technical proposal", "architecture doc"
    - "explainer", "explain X visually", "interactive explainer"
    - "strategy doc", "decision framework", "post-mortem", "retro", "case study"
    - "make a doc/page with [a chart / simulation / slider / model / diagram]"
    - "create a webpage to explain X", "publish this as HTML"
    - "I want people to comment on this", "let people read and comment"
    - editing or updating an existing doc/site/page the user previously made

  When a request names an existing doc (e.g. "update the plaud explainer",
  "address the comment on the X writeup"), that is an EDIT request — pull
  the comments and regenerate.

  Use this INSTEAD of raw markdown / Google-Docs-style output when the user
  wants something interactive, shareable via URL with commenting, or that
  benefits from being a real HTML page. Also use it for agent-originated
  docs you decide to emit mid-flow (release notes, retros, post-mortems,
  investigation/audit/QA reports, design critiques, research write-ups).
allowed-tools:
  - Bash
  - Read
  - Write
  - Edit
  - Glob
triggers:
  - write a doc
  - draft a doc
  - make a doc
  - write something up
  - document this
  - publish this
  - share this writeup
  - make it shareable
  - research write-up
  - product spec
  - PRD
  - one-pager
  - design doc
  - RFC
  - technical proposal
  - architecture doc
  - explainer
  - interactive explainer
  - post-mortem
  - case study
  - create a webpage
  - publish as HTML
  - let people read and comment
---

# octo-doc — prompt-native HTML documents (API-first)

Docs are HTML build artifacts, not files the user maintains. The authoring
interface is a prompt; the agent turns it into a self-contained HTML page and
drives the doc lifecycle over the octo-doc **`/v1` HTTP API**. Authoring is
**remote-first**: a doc lives on the server from creation as a mutable **draft**,
and promoting the draft mints an immutable version. Comments anchor to highlighted
text or to artifacts (images, SVG, canvas, video) and drive the next iteration.
Docs are **private by default** — the write token is the author; a per-doc share
**code** grants read + comment.

**The mechanics are just API calls.** `scripts/octo.sh` (bash + curl + jq) wraps
them so each step is one command; the agent's real job is the creative part —
turning a prompt into HTML and deciding how to address comments. There is no
compiled CLI and no local preview server; you author against a running octo-doc
server (a hosted instance or the local Docker stack). The raw endpoints are in
**[references/api.md](references/api.md)** — use `curl` directly if you prefer.

## Configuration

The helper and any `curl` you run read three env vars:

| Var | Purpose |
| --- | ------- |
| `OCTO_BASE_URL` | server to author against, e.g. `https://docs.example.com` (required) |
| `OCTO_TOKEN` | write token — the **author** credential (`Authorization: Bearer`) |
| `OCTO_CODE` | a per-doc share **code** — the **reader** credential (pull/comment) |

```bash
export OCTO_BASE_URL="https://docs.example.com"
export OCTO_TOKEN="$(scripts/octo.sh bootstrap)"   # first run on a fresh server
```

`bootstrap` mints the first write token (only while the server has no static
`WRITE_TOKEN` and hasn't been bootstrapped). Treat the token as a secret — it
belongs in the `Authorization: Bearer` header, never in a URL or a shared log.

## Access model

- **author** = the write token: read everything incl. drafts; publish, promote,
  delete; mint/rotate share codes; upload assets; post agent replies.
- **reader** = a per-doc share code: read published versions + comment/react.
  Never drafts, publishing, or deletion.
- no credential → the server returns **404** (a private doc never confirms it
  exists). Full model:
  [docs/AUTH.md](https://github.com/Mininglamp-OSS/octo-docs-html/blob/main/docs/AUTH.md).

## Setup check

```bash
curl -fsS "$OCTO_BASE_URL/v1/ping" >/dev/null && echo "server reachable" \
  || echo "set OCTO_BASE_URL to a running octo-doc server"
```

## Create a doc

1. Pick a slug from the prompt (kebab-case, ≤4 words, `^[a-zA-Z0-9_-]{1,64}$`).
2. Author a **fully self-contained** HTML file. Start from
   [templates/doc.html](templates/doc.html) and follow
   [references/authoring.md](references/authoring.md): one file, inline CSS/JS,
   **do NOT re-style** (the overlay injects the default template), a required
   `.wrap` container, responsive defaults. If the prompt implies a model,
   simulation, or diagram, **build the live thing** — don't just describe it.
   For binary media (images/video/PDF) upload assets instead of base64-inlining
   (see Assets below).
3. Save it as the doc's **draft** (private, author-only) or publish it directly:
   ```bash
   # save a draft to iterate on (renders at /d/<slug>/draft, author-only):
   scripts/octo.sh draft <slug> ./doc.html "Human Title"

   # …or publish straight to an immutable v1:
   scripts/octo.sh publish <slug> ./doc.html "Human Title"   # → {url:/d/<slug>/v/1}
   ```
4. Report the URL. The draft is at `/d/<slug>/draft`; a published version at
   `/d/<slug>/v/<n>`. When a draft is ready, promote it:
   ```bash
   scripts/octo.sh promote <slug>            # draft → next immutable version
   ```

Keep the generated HTML on disk (e.g. `./<slug>.html`) — you'll read it back to
edit. The server is the source of truth for versions and comments.

## Edit a doc (new version from comments)

You MUST report back on every open comment — applied, partial, or unclear. The
user can't tell which comments you handled unless you reply on each one. Skipping
comments silently is the #1 source of regression complaints.

1. Pull the full comment history and filter to `status: "open"`:
   ```bash
   scripts/octo.sh pull <slug> > /tmp/<slug>.comments.json
   ```
   Each comment carries an `anchor` (see [references/anchoring.md](references/anchoring.md)):
   - `anchor.text` — exact highlighted text (may span elements)
   - `anchor.context_before` / `context_after` — ~60 chars each side to disambiguate
   - element anchors carry an `aid` (identity-based; stable across versions)
2. Read the current HTML (your on-disk copy, or fetch a version render).
3. For EACH open comment decide **one** outcome before writing:
   - **applied** — clear and actionable.
   - **partial** — applied part; couldn't fully address it.
   - **question** — can't act without clarification.
4. Regenerate the HTML incorporating every `applied`/`partial` comment, then save
   it as the new draft and promote:
   ```bash
   scripts/octo.sh draft <slug> ./doc.html "Human Title"
   scripts/octo.sh promote <slug>
   ```
5. **Post an agent reply on every comment** (mandatory) so the user sees the
   outcome in the doc UI. The reply flips the comment's status server-side and
   drops a status emoji (✅ applied / 🟡 partial / ❓ question):
   ```bash
   scripts/octo.sh reply <slug> <comment_id> "Rewrote §2 in plain English." applied <new-version>
   scripts/octo.sh reply <slug> <comment_id> "Added the chart; explainer still thin — flesh out?" partial
   scripts/octo.sh reply <slug> <comment_id> "Two comments want different tones — which wins?" question
   ```
   Be specific in the text. If the user later re-anchors a comment, the server
   resets it to `open` and the next edit pass picks it up again.

If there are zero open comments and no extra prompt, ask the user what to change.

## Share a doc

New docs are private. Mint a read + comment link and hand it out:

```bash
scripts/octo.sh share <slug>       # → { code, url: .../d/<slug>/v/<n>?code=<code> }
scripts/octo.sh unshare <slug>     # revoke — existing links stop working
```

Re-running `share` rotates the code. Anyone with the `?code=` link gets read +
comment; it never grants publishing or deletion.

## Assets (images, video, PDF)

Don't base64-inline binary media (bloats the doc, capped by `MAX_HTML_BYTES`) or
hot-link a third-party CDN. Upload it as a per-doc asset and reference the returned
same-origin URL from your HTML:

```bash
scripts/octo.sh asset-add <slug> ./chart.png    # → { url: /d/<slug>/assets/<sha> }
scripts/octo.sh assets <slug>                   # list a doc's assets
```

```html
<img src="/d/<slug>/assets/<sha>" alt="…">
<video src="/d/<slug>/assets/<sha>" controls></video>
<object data="/d/<slug>/assets/<sha>" type="application/pdf" width="100%" height="800"></object>
```

## Other operations

```bash
scripts/octo.sh versions <slug>       # list published versions
scripts/octo.sh comment <slug> "text" <version>   # post a doc-level comment
scripts/octo.sh react <slug> <comment_id> 👍       # toggle a reaction
scripts/octo.sh unpublish <slug>      # delete the doc (all versions + comments + assets)
scripts/octo.sh render-url <slug> <n> # print the doc URL
scripts/octo.sh help                  # full command list
```

## Troubleshooting

- **A doc URL 404s** → docs are private by default. Open the `?code=` share link,
  or authenticate as the author. A wrong/rotated code also 404s.
- **`OCTO_BASE_URL` not set / server unreachable** → the helper exits early; point
  it at a running server (`GET /v1/ping` should return `service: octo-doc`).
- **401 / "author op" error** → the write token is wrong or missing; set
  `OCTO_TOKEN` (mint one with `scripts/octo.sh bootstrap` on a fresh server).
- **413 payload too large** → the doc exceeds `MAX_HTML_BYTES` (default 5 MiB) —
  move large media to assets.
- **415 unsupported_media_type** on `asset-add` → the file's sniffed MIME isn't in
  the server's `ASSET_MIME_ALLOW`.

## References

- **[references/api.md](references/api.md)** — the full `/v1` endpoint reference
  (methods, auth, request bodies, response envelope, error codes). The helper is a
  thin wrapper over these; use `curl` directly when you need something it doesn't
  cover.
- **[references/authoring.md](references/authoring.md)** — HTML generation rules,
  the default-styling contract (do NOT re-style), required container structure,
  responsive defaults, overlay-conflict rules, comment-anchor stability. Start
  from **[templates/doc.html](templates/doc.html)**.
- **[references/anchoring.md](references/anchoring.md)** — the comment anchor JSON
  shapes (text / element / lost) and how to interpret them when editing.
