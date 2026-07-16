# Authoring HTML docs

Rules for generating a doc's HTML (referenced by the Create and Edit flows in
SKILL.md). The starting skeleton is [`templates/doc.html`](../templates/doc.html).

## HTML generation rules

- **Self-contained:** one HTML file. No imports, no external scripts (unless the
  user explicitly wants e.g. a D3 CDN). All CSS inline in `<style>`, all JS inline
  in `<script>`. No build step.
- **Large media → per-doc assets, not base64/hot-link.** Inline small SVG/diagrams
  you author directly. For binary media (images, audio, video, PDF), upload it via
  `scripts/octo.sh asset-add <slug> <file>` (or `POST /v1/docs/<slug>/assets`) and
  reference the returned same-origin URL (`/d/<slug>/assets/<sha>`) — don't
  base64-inline it (bloats the doc, bounded by `MAX_HTML_BYTES`) or hot-link a
  third-party CDN.
- **Sandboxed-safe:** the server serves docs inside an iframe overlay-host, so
  don't rely on top-level navigation or parent-frame access.
- **The comment overlay is injected by the server** — don't add commenting UI
  yourself.
- **Don't add** a "made with octo" footer, version selector, or share button —
  the shell handles those.
- Prefer **SVG over canvas** for diagrams (commentable text). Use canvas for heavy
  simulations.
- If the prompt implies a model, simulation, or diagram, **build the live thing** —
  don't just describe it.
- Default font stack: `system-ui, -apple-system, "Segoe UI", Roboto, sans-serif`.
  Mono: `ui-monospace, "SF Mono", Menlo, monospace`.

## Default styling — DO NOT re-style the doc

The overlay injects a complete default template (tight, readable, system fonts
only):

- System font stack; body 17px / line-height 1.65 / `#111` on white
- h1 34px / 1.15 / -0.01em; h2 24px / 1.25 / 40px top margin; h3 19px / 1.35 / 28px top margin
- Paragraph 18px bottom margin
- Blockquote: 3px solid `#111` left rule, light quoted block
- `pre`: mono 15px, light-gray background, left-rule, scrolling overflow
- inline code: 0.92em mono, light-gray rounded chip

**Don't write your own CSS for these** unless the doc genuinely needs a different
aesthetic (a presentation, a landing page, a doc with custom widgets). Reading
docs, essays, and reports should not override the template. The overlay's
`:where()` defaults handle the centered 720px column, headings, lists, code/pre,
blockquote, tables, link color, and image margins.

Only add CSS for **doc-specific** content, scoped tightly (`.my-slider { ... }`,
not `body p { ... }`).

## Required container structure

Wrap content in a single container with one of: **`.wrap`** (preferred), `main`,
`article`, `.content`, or `.container`. The overlay relies on it to detect article
width for the responsive breakpoint, anchor the article left when comments exist,
and place comment cards.

Do **not** give the container `margin: 0 auto` — the overlay sets its margins
based on comment state (and overrides with `!important` if you write it).

## Required: explicit body background

Always set `body { background: #fff; }` (or your chosen color) so the page isn't
transparent over the browser default. **Light mode only** — the overlay does not
currently support dark mode.

## Responsive defaults (REQUIRED)

Every doc must work on mobile out of the box:

- **Always include** `<meta name="viewport" content="width=device-width, initial-scale=1">`.
- **Fluid widths**, not hardcoded pixels. Container: `max-width: 720px; padding: 0 24px;`.
- **Canvas / SVG / images:** don't hardcode `width=N height=M`. Use `width="100%"`
  + CSS `aspect-ratio`, or a `max-width: 100%` wrapper. For canvas, set the
  `width`/`height` attributes for the drawing buffer but also
  `style="max-width:100%;height:auto"`; recompute the buffer on resize if needed.
- **Tables:** wrap in `<div style="overflow-x:auto">`.
- **`<pre>`:** `max-width: 100%; overflow-x: auto;`.
- **Test at 375px wide** before claiming done.

## Don't conflict with the overlay's UI

- **Don't define `button:hover { background: ... }` globally** — it overrides the
  overlay's Comment pill on artifacts. Scope hover rules (`.my-btn:hover`).
- **Don't use `odoc-*` / `#odoc-*` ids or classes** — reserved by the overlay.
- **Don't position-fixed elements at the top** — the overlay's 44px top bar lives there.
- **Don't add a bottom footer** — the overlay injects its own.

## Comment anchor stability (important when editing)

**The system handles this for you.** Element anchors are identity-based, not
path-based: at publish time the server stamps every commentable artifact with a
content-hashed `data-odoc-aid`. Commentable artifacts:

- **Media leaves:** `img, svg, canvas, video, pre, figure, iframe[src]`
- **Semantic blocks:** `section, aside, blockquote, table, details` (`article` is
  intentionally excluded — it would make the whole doc one artifact)
- **Author opt-in:** any element tagged `data-odoc-artifact` or with a class
  containing `odoc-artifact`

The **same artifact in any future version gets the same aid**, regardless of how
the surrounding HTML is restructured. Resolution is identity-first. If an aid
disappears, the server marks the comment `kind: "lost"` (renders unanchored) —
it **never silently re-attaches to a different artifact**.

### Make an author-composed block commentable as a unit

A "card"/composite widget built from `<div>`s isn't commentable as a unit by
default (the overlay sees inner text, not the card). Two fixes:

1. **Semantic tag:** `<div class="my-card">` → `<section class="my-card">` (or
   `<aside>`, `<details>`). Automatic.
2. **Opt in:** `<div class="my-card" data-odoc-artifact>…</div>` (or a class
   containing `odoc-artifact`). Works on any tag.

Both give the block a stable aid and the full hover-to-comment affordance.

When regenerating you usually need do nothing special (aid stamping is automatic
on publish), but it's polite to:

- **Keep an artifact's essential content stable** if its thread still matters. The
  aid derives from tag + intrinsic attrs (`viewBox`, `src`, `alt`, `aria-label`,
  `title`) + normalized inner content. Whitespace changes don't matter; replacing
  an SVG with a different one *does* (correctly — the comments were about the old one).
- **Stable author ids** are nice for deep links but no longer required for anchoring.
- **When a comment intentionally goes unanchored** (you replaced the artifact),
  say so in the agent reply so the user knows to re-anchor or accept the loss.
