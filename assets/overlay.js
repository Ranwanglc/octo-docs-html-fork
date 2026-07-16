// odoc overlay — single-file design.
// Sections are demarcated with `// ========== Name ==========` headers so the
// file reads like several concatenated modules. Each section depends only on
// the ones above it (and on `state`). No section reaches sideways.
//
// External contract preserved verbatim:
//   - Endpoints: /v1/comments, /v1/reactions, /v1/auth/logout,
//     /d/<slug>/v/<n>/export
//   - Globals: window.__odocCopyDocMd(includeComments), window.__odocCopyCommentMd(id, btn)
//   - Body classes: odoc-has-comments, odoc-narrow
//   - Keyboard: ⌘/Ctrl-Enter submits, Esc cancels.
//
// Highlight rendering: CSS Custom Highlight API (CSS.highlights). One named
// highlight `odoc-pending` for the in-flight selection, and one
// `odoc-anchor-<id>` per saved comment. This replaces the legacy
// surroundContents/extractContents path that produced empty yellow bars when
// the selection crossed block boundaries. A minimal single-textnode <span>
// fallback runs on browsers without `CSS.highlights`.

(function () {
  // ========== Config & DOM setup ==========
  const cfg = window.__ODOC__ || {};
  const { slug, version } = cfg;
  const mode = cfg.mode || 'local';
  const isPublished = mode === 'published';
  const isFork = mode === 'fork';
  const isLocal = mode === 'local';
  // Draft mode: an author previewing an unpublished doc on the server. The CTA is
  // "Publish" (promote the draft to an immutable version), not Share.
  const isDraft = mode === 'draft';
  // Author vs reader (carried outside the byte-frozen __ODOC__ config). Only the
  // author (write token) may mint/rotate a share code, so the Share CTA is shown
  // only to them — a reader clicking it would 404 on POST /share.
  const isAuthor = !!(window.__ODOC_CAP__ && window.__ODOC_CAP__.isAuthor);
  // Fork mode renders the doc read-only with comments mirrored from the
  // embedded #odoc-fork-comments JSON. No /api calls, no auth, no publish.
  // The original published slug is in cfg.originalSlug so we can label it.
  let identity = cfg.identity || null;
  let isOwner = !!cfg.isOwner; // true only for the configured ODOC_OWNER
  // Whether a login provider is configured server-side. When false, commenting is
  // anonymous (no sign-in UI, no gating). A future Octo unified login flips this
  // on and the existing identity/session paths light up unchanged.
  const authConfigured = !!cfg.authConfigured;
  if (!slug) return;

  const HIGHLIGHT_API = typeof CSS !== 'undefined' && CSS.highlights && typeof Highlight === 'function';

  // Phones need this or they render at a virtual ~980px viewport.
  if (!document.querySelector('meta[name="viewport"]')) {
    const m = document.createElement('meta');
    m.name = 'viewport';
    m.content = 'width=device-width, initial-scale=1, viewport-fit=cover';
    document.head.appendChild(m);
  }

  // ========== UI selector registry ==========
  // One source of truth for "is this part of the odoc overlay UI?".
  //   UI_CONTAINERS — top-level overlay regions: bar, popups, comment column,
  //                   margin cards, modals, footer. Use these when finding the
  //                   doc's article element or stripping the overlay from a
  //                   clone for export.
  //   UI_ALL        — UI_CONTAINERS plus per-element decorations (anchor marks,
  //                   outlines, hover affordances, menus). Use this for event
  //                   delegation guards ("did the user click *our* chrome?").
  const UI_CONTAINERS = '.odoc-bar, .odoc-oldver-strip, .odoc-popup, .odoc-margin-comment, .odoc-modal-bg, #odoc-comment-layer, .odoc-footer, .odoc-reanchor-banner';
  const UI_ALL = UI_CONTAINERS + ', .odoc-anchor-mark, .odoc-element-outline, .odoc-hover-outline, .odoc-comment-pill, .odoc-emoji-picker, .odoc-secondary-menu';
  //   UI_TEXT       — every overlay element that carries its own visible text
  //                   AND lives outside the article (appended to <body>). Used
  //                   ONLY to exclude our chrome from the flattened text
  //                   projection, so saved anchor context never captures UI
  //                   strings (the FAB's "💬 0", the re-anchor banner, the
  //                   Comment pill, menus). This is UI_CONTAINERS plus the
  //                   text-bearing body-level widgets — but NOT .odoc-anchor-mark,
  //                   which wraps real document text (surroundContents) and must
  //                   stay in the projection.
  const UI_TEXT = UI_CONTAINERS + ', .odoc-fab, .odoc-comment-pill, .odoc-emoji-picker, .odoc-secondary-menu';

  // ========== Geometry helpers ==========
  // Position `box` as an absolutely-positioned overlay around `el`, inflated
  // by `inset` pixels on each side (default 3 → a 3px-wide outline ring).
  function positionOutlineAround(box, el, inset = 3) {
    const r = el.getBoundingClientRect();
    box.style.top = (window.scrollY + r.top - inset) + 'px';
    box.style.left = (window.scrollX + r.left - inset) + 'px';
    box.style.width = (r.width + inset * 2) + 'px';
    box.style.height = (r.height + inset * 2) + 'px';
  }

  // ========== Styles ==========
  // Each logical group is one comment block; rules within a group are tightly
  // packed. The narrow visual mode lives at the bottom and overrides base.
  const css = `
  /* ========== Design tokens ==========
     Single source of truth for the overlay's visual identity. Values mirror
     DESIGN.md (repo root) — keep the two in sync. Every .odoc-* chrome rule
     below references these vars rather than hardcoding hex, so a rebrand is a
     one-block edit. */
  :root {
    /* Brand + ink */
    --octo-primary: #1652f0;
    --octo-primary-hover: #1245d0;
    --octo-ink: #1a1a1a;
    --octo-ink-strong: #111;
    --octo-ink-soft: #444;
    --octo-muted: #888;
    --octo-muted-dark: #666;
    --octo-faint: #aaa;
    --octo-bar-text: #555;
    /* Surfaces + lines */
    --octo-surface: #fff;
    --octo-surface-subtle: #f0f1f4;
    --octo-surface-hover: #e5e6ea;
    --octo-border: #e5e5e7;
    --octo-hairline: #eee;
    --octo-ink-panel: #0a0a0a;
    /* Document-body neutrals (default template) */
    --octo-code-bg: #f0f0ee;
    --octo-pre-bg: #f7f7f5;
    --octo-pre-border: #e8e7e3;
    --octo-quote-rule: #d9d8d3;
    --octo-quote-text: #6b6a66;
    /* Accents */
    --octo-highlight: #fff7d0;
    --octo-highlight-active: #ffd84d;
    --octo-danger: #c33;
    --octo-accent: #3ecf8e;
    /* Semantic — agent status */
    --octo-ok-bg: #e8f5ed;    --octo-ok-fg: #1a7340;
    --octo-warn-bg: #fff4dc;  --octo-warn-fg: #8a5a00;
    --octo-ask-bg: #ffe7e7;   --octo-ask-fg: #a52323;
    /* Semantic — reactions */
    --octo-mine-bg: #e8eeff;
    --octo-agent-bg: #f3eaff; --octo-agent-border: #c3a8f0; --octo-agent-fg: #5a2da8;
    /* Radii */
    --octo-radius-sm: 4px;
    --octo-radius-md: 6px;
    --octo-radius-lg: 10px;
    --octo-radius-xl: 12px;
    --octo-radius-pill: 999px;
    /* Elevation (DESIGN.md Elevation section) */
    --octo-shadow-sm: 0 1px 2px rgba(0,0,0,0.02);
    --octo-shadow-card: 0 2px 8px rgba(0,0,0,0.05);
    --octo-shadow-menu: 0 8px 24px rgba(0,0,0,0.12);
    --octo-shadow-pop: 0 12px 40px rgba(0,0,0,0.4);
    --octo-shadow-fab: 0 4px 16px rgba(22,82,240,0.35);
    --octo-shadow-active: 0 4px 16px rgba(22,82,240,0.18);
    --octo-focus-ring: 0 0 0 3px rgba(22,82,240,0.15);
  }
  /* Layout */
  /* Keyboard focus: one consistent ring across all interactive chrome, so
     tabbing through the overlay is visible and accessible. :focus-visible only
     shows it for keyboard users, not on mouse click. */
  .odoc-bar button:focus-visible, .odoc-version-toggle:focus-visible,
  .odoc-chip:focus-visible, .odoc-menu button:focus-visible,
  .odoc-secondary-menu button:focus-visible, .odoc-version-menu button:focus-visible,
  .odoc-emoji-picker button:focus-visible, .odoc-react-chip:focus-visible,
  .odoc-react-add:focus-visible, .odoc-reanchor-btn:focus-visible,
  .odoc-reply-toggle:focus-visible, .odoc-comment-pill:focus-visible,
  .odoc-fab:focus-visible, .odoc-margin-comment .copy-md:focus-visible,
  .odoc-margin-comment .del:focus-visible, .odoc-popup .submit:focus-visible,
  .odoc-popup .head .x:focus-visible, .odoc-reply-submit:focus-visible,
  .odoc-modal button:focus-visible {
    outline: none !important;
    box-shadow: var(--octo-focus-ring) !important;
  }
  /* Default: text is selectable everywhere in the document body, so users
     can highlight prose inside any container (including custom-div-wrapped
     artifacts like transcript panes). UI chrome opts out explicitly via
     .odoc-* selectors below. Media artifacts (img/svg/canvas/video) are
     non-selectable by their nature so they don't need an exception. */
  body { padding-top: 49px !important; padding-bottom: 24px; -webkit-user-select: text; user-select: text; }
  body .odoc-bar, body .odoc-bar *, body #odoc-comment-layer, body #odoc-comment-layer *, body .odoc-hover-outline, body .odoc-comment-pill, body .odoc-emoji-picker, body .odoc-secondary-menu, body .odoc-anchor-mark.odoc-anchor-mark-element, body .odoc-drag-marquee, body .odoc-modal, body .odoc-modal * { -webkit-user-select: none !important; user-select: none !important; }
  body .odoc-modal .code, body .odoc-modal textarea, body .odoc-modal input { -webkit-user-select: text !important; user-select: text !important; }
  /* Reserve the 320px comment column on the right. The article centers
     itself inside the remaining (viewport - 320px) space via margin auto
     (applied below in :where()). Adding a left padding keeps it from
     hugging the screen edge on wide windows. */
  body.odoc-has-comments:not(.odoc-narrow) { padding-right: 320px !important; padding-left: 80px !important; }
  body.odoc-narrow { padding-right: 0 !important; }
  /* Center the article container in the reading column. :where() so any
     doc-defined margin wins. Applies only on wide layouts; narrow mode
     uses the full body width via the drawer. */
  body:not(.odoc-narrow) :where(body > .wrap, body > main, body > article, body > .content, body > .container) {
    margin-left: auto !important;
    margin-right: auto !important;
  }
  /* Bare docs (no wrapper element — content is a direct child of <body>) still
     get a comfortable centered reading measure, so prose never hugs the viewport
     edge. Scoped with :not(:has(wrapper)) so wrapped docs (handled above) aren't
     double-inset, and skipped when comments reserve the right column (that path
     already pads the body) or in narrow/drawer mode. */
  body:not(.odoc-has-comments):not(.odoc-narrow):not(:has(> .wrap, > main, > article, > .content, > .container)) {
    padding-left: max(24px, calc((100% - 720px) / 2)) !important;
    padding-right: max(24px, calc((100% - 720px) / 2)) !important;
    padding-top: 73px !important;
  }
  body.odoc-narrow:not(:has(> .wrap, > main, > article, > .content, > .container)) {
    padding-left: 20px !important;
    padding-right: 20px !important;
  }
  /* The body right-padding reserves space for the comment column. The
     article centers itself naturally inside the remaining (viewport minus
     320px) space via its own margin auto. As the window shrinks, the symmetric
     margins shrink with it; once they hit the article's min width, narrow-mode
     takes over and the drawer kicks in. */
  /* ========== Default doc template (single typography template) ==========
     One canonical look for every odoc doc: same font stack, sizes, spacing,
     headings, lists, code, tables, quotes. Wrapped in :where() so a doc that
     truly needs a different aesthetic can override per element. Future
     templates would live alongside this block, switched by a body class. */
  /* Default template, modeled after Claude Code's markdown rendering.
     Readable, system-fonts, rounded-cell tables, circle task checkboxes. */
  :where(body) {
    font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    font-size: 17px;
    line-height: 1.6;
    color: var(--octo-ink);
    background: var(--octo-surface);
    text-rendering: optimizeLegibility;
    -webkit-font-smoothing: antialiased;
    overflow-wrap: anywhere;
  }
  :where(body h1) { font-size: 38px; line-height: 1.15; font-weight: 700; letter-spacing: -0.02em; margin: 0 0 20px; color: var(--octo-ink); }
  :where(body h2) { font-size: 27px; line-height: 1.25; font-weight: 700; letter-spacing: -0.01em; margin: 44px 0 14px; color: var(--octo-ink); }
  :where(body h3) { font-size: 21px; line-height: 1.35; font-weight: 700; margin: 32px 0 10px; color: var(--octo-ink); }
  :where(body h4) { font-size: 17px; font-weight: 700; margin: 22px 0 6px; color: var(--octo-ink); }
  :where(body h5, body h6) { font-size: 14px; font-weight: 600; margin: 16px 0 4px; color: var(--octo-ink); text-transform: uppercase; letter-spacing: 0.06em; }
  :where(body p) { margin: 0 0 16px; }
  :where(body a) { color: var(--octo-primary); text-decoration: underline; text-underline-offset: 2px; overflow-wrap: anywhere; }
  :where(body a:hover) { text-decoration-thickness: 2px; }
  :where(body ul, body ol) { margin: 0 0 18px; padding-left: 26px; }
  :where(body li) { margin: 8px 0; }
  :where(body blockquote) { margin: 20px 0; padding: 2px 0 2px 20px; border-left: 3px solid var(--octo-quote-rule); color: var(--octo-quote-text); }
  :where(body code) { font-family: ui-monospace, "SF Mono", Menlo, monospace; font-size: 0.88em; background: var(--octo-code-bg); padding: 2px 6px; border-radius: var(--octo-radius-md); overflow-wrap: anywhere; }
  :where(body pre) { font-family: ui-monospace, "SF Mono", Menlo, monospace; font-size: 14.5px; line-height: 1.6; background: var(--octo-pre-bg); border: 1px solid var(--octo-pre-border); border-radius: var(--octo-radius-lg); padding: 16px 18px; margin: 20px 0; overflow-x: auto; }
  :where(body pre code) { background: transparent; padding: 0; border-radius: 0; }
  :where(body hr) { border: 0; border-top: 1px solid var(--octo-pre-border); margin: 36px 0; }
  /* Tables: minimal divider style — a hairline under each row, a slightly
     stronger rule under the header, and a soft hover on body rows. No cell
     backgrounds or per-cell borders, so it stays clean at any width. */
  :where(body table) { border-collapse: collapse; width: 100%; margin: 0 0 20px; font-size: 15.5px; }
  :where(body th, body td) { padding: 11px 14px; text-align: left; border-bottom: 1px solid var(--octo-border); vertical-align: top; }
  :where(body thead th) { font-size: 12px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.04em; color: var(--octo-muted); border-bottom: 1.5px solid var(--octo-quote-rule); }
  :where(body tbody tr):last-child :where(td) { border-bottom: 0; }
  :where(body tbody tr):hover { background: var(--octo-surface-subtle); }
  :where(body figcaption) { font-size: 13px; color: var(--octo-quote-text); margin-top: 6px; text-align: center; }
  /* Task lists: circle checkboxes, Claude Code style. Works for raw
     <input type=checkbox> in lists and markdown-converted .task-list-item. */
  :where(body li:has(> input[type="checkbox"]), body li.task-list-item) { list-style: none; margin-left: -26px; }
  :where(body input[type="checkbox"]) {
    appearance: none; -webkit-appearance: none;
    width: 17px; height: 17px;
    border: 1.5px solid #c9c8c3; border-radius: 50%;
    vertical-align: -3px; margin: 0 8px 0 0;
    background: var(--octo-surface); cursor: default;
  }
  :where(body input[type="checkbox"]:checked) {
    background: var(--octo-ink) center / 11px no-repeat url('data:image/svg+xml;utf8,<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><path d="M3 8.5l3.5 3.5L13 5" stroke="white" stroke-width="2.2" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>');
    border-color: var(--octo-ink);
  }
  /* Doc imagery only — exclude overlay UI so icons inside the bar / chips /
     buttons / cards keep their inline layout instead of stacking to 16px tall. */
  :where(body img, body svg, body canvas, body video):not(.odoc-bar *):not(.odoc-margin-comment *):not(.odoc-popup *):not(.odoc-modal-bg *):not(.odoc-chip *):not(.odoc-fab *):not(#odoc-comment-layer *):not(.odoc-footer *) { display: block; margin: 16px auto; border-radius: var(--octo-radius-md); }
  /* Reading column for the doc container. :where() so a doc's own rule wins. */
  :where(body > .wrap, body > main, body > article, body > .content, body > .container) {
    max-width: 720px;
    padding: 56px 24px 80px;
    box-sizing: border-box;
  }
  /* End default template. ====================================================== */

  /* Defensive responsive defaults for artifacts. Docs sometimes hardcode pixel
     widths (e.g. <canvas width="640">) that overflow on phones. These rules
     constrain every artifact to its container width without changing its
     aspect ratio. Wrapped in :where() so the doc's own CSS wins if specified. */
  :where(body img, body video, body iframe, body svg, body canvas) {
    max-width: 100% !important;
    height: auto;
    box-sizing: border-box;
  }
  /* Canvas needs special handling: scaling its CSS size doesn't change its
     drawing-buffer size, but at least the box won't overflow. */
  :where(body canvas) { display: block; }
  /* Wide tables: keep TRUE table layout on desktop — display:block on a
     table element discards real table layout for anonymous-box fixup, which
     some engines render with uneven row heights and gaps (seen on published
     docs). Only degrade to a scrollable block on narrow viewports, where
     horizontal overflow is the bigger evil. NOTE: no backticks in comments
     here — this CSS lives inside a JS template literal. */
  :where(body table) { max-width: 100%; }
  @media (max-width: 760px) {
    :where(body table) { display: block; overflow-x: auto; }
  }
  /* Pre/code blocks scroll horizontally instead of breaking the layout. */
  :where(body pre) { max-width: 100%; overflow-x: auto; }

  /* ========== Top bar (HackMD-inspired rhythm) ==========
     Three groups: left breadcrumb (workspace + slug + version), center
     doc title (truncates), right cluster (identity, primary CTA, more).
     No borders on individual buttons — uses hover background instead, so
     the bar reads as a clean strip rather than a row of chiclets.
     Light theme to match the doc body. */
  .odoc-bar { position: fixed; top: 0; left: 0; right: 0; height: 48px; background: var(--octo-surface); color: var(--octo-ink); display: flex; align-items: center; padding: 0 12px; font: 13px system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; z-index: 999999; gap: 8px; border-bottom: 1px solid var(--octo-border); box-shadow: var(--octo-shadow-sm); }
  .odoc-bar-left { display: flex; align-items: center; gap: 6px; min-width: 0; flex-shrink: 1; }
  .odoc-bar-center { flex: 1 1 auto; display: flex; justify-content: center; min-width: 0; padding: 0 8px; }
  .odoc-bar-right { display: flex; align-items: center; gap: 4px; flex-shrink: 0; }

  /* Workspace mark — token-driven o·d monogram (inline SVG). Clicks → /. */
  .odoc-bar-mark { display: inline-flex; align-items: center; justify-content: center; height: 30px; width: 30px; padding: 3px !important; border-radius: var(--octo-radius-md); background: transparent; cursor: pointer; flex-shrink: 0; border: none; transition: background .12s; }
  .odoc-bar-mark svg { width: 24px; height: 24px; display: block; }
  .odoc-bar-mark:hover { background: var(--octo-surface-subtle); }

  /* Breadcrumb: workspace · slug · v3 — separated by " / ". */
  .odoc-bar .crumb { color: var(--octo-bar-text); font-weight: 500; padding: 4px 6px; border-radius: var(--octo-radius-md); max-width: 24ch; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .odoc-bar .crumb-sep { color: var(--octo-faint); user-select: none; padding: 0 1px; }
  .odoc-bar .doc-title { color: var(--octo-ink); font-weight: 600; font-size: 14px; max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

  /* Default action button — icon and/or label, no border, hover bg only. */
  .odoc-bar button { background: transparent; border: none; color: var(--octo-bar-text); padding: 6px 8px; border-radius: var(--octo-radius-md); font: inherit; cursor: pointer; transition: background .12s, color .12s; display: inline-flex; align-items: center; gap: 6px; }
  .odoc-bar button:hover { background: var(--octo-surface-subtle); color: var(--octo-ink); }
  .odoc-bar button:disabled { opacity: 0.5; cursor: not-allowed; }
  .odoc-bar button svg { flex-shrink: 0; }

  /* Primary CTA (Share / Publish) — filled blue button at the right. */
  .odoc-bar button.primary { background: var(--octo-primary); color: var(--octo-surface); padding: 7px 14px; font-weight: 600; box-shadow: 0 1px 2px rgba(22,82,240,0.2); }
  .odoc-bar button.primary:hover { background: var(--octo-primary-hover); color: var(--octo-surface); box-shadow: 0 2px 8px rgba(22,82,240,0.28); }

  /* Version picker chip — pill in the left breadcrumb. */
  .odoc-version-wrap { position: relative; display: inline-block; flex-shrink: 0; }
  .odoc-version-toggle { background: var(--octo-surface-subtle) !important; color: var(--octo-ink) !important; padding: 3px 10px !important; border-radius: var(--octo-radius-pill) !important; font: 12px ui-monospace, "SF Mono", Menlo, monospace !important; }
  .odoc-version-toggle:hover { background: var(--octo-surface-hover) !important; }

  /* Dropdown menus — light surface to match the bar. */
  .odoc-menu, .odoc-secondary-menu, .odoc-version-menu { display: none; position: absolute; background: var(--octo-surface); border: 1px solid var(--octo-border); border-radius: var(--octo-radius-lg); padding: 4px; box-shadow: var(--octo-shadow-menu); z-index: 1000000; min-width: 160px; }
  .odoc-version-menu { top: calc(100% + 6px); left: 0; max-height: 60vh; overflow-y: auto; }
  .odoc-menu { top: calc(100% + 6px); right: 0; min-width: 180px; }
  .odoc-secondary-menu { top: calc(100% + 6px); right: 0; }
  .odoc-menu.open, .odoc-secondary-menu.open, .odoc-version-menu.open { display: block; }
  .odoc-menu button, .odoc-secondary-menu button, .odoc-version-menu button { display: block; width: 100%; text-align: left; padding: 7px 10px; border-radius: var(--octo-radius-sm); color: var(--octo-ink); font: 13px system-ui, sans-serif; }
  .odoc-version-menu button { font-family: ui-monospace, "SF Mono", Menlo, monospace; }
  .odoc-menu button:hover, .odoc-secondary-menu button:hover, .odoc-version-menu button:hover { background: var(--octo-surface-subtle); }
  .odoc-version-menu button.current { color: var(--octo-primary); font-weight: 600; }

  .odoc-menu-wrap { position: relative; display: inline-block; }
  /* Overflow ⋯ button shows on narrow viewports. */
  .odoc-bar .odoc-secondary-toggle { display: none; padding: 6px 10px; }

  /* Identity chip — avatar + name (name hides on narrow). */
  .odoc-chip { display: inline-flex; align-items: center; gap: 8px; padding: 3px 12px 3px 3px; background: var(--octo-surface-subtle); border-radius: var(--octo-radius-pill); cursor: pointer; color: var(--octo-ink); font: inherit; border: none; }
  .odoc-chip:hover { background: var(--octo-surface-hover); }
  .odoc-chip img { width: 26px; height: 26px; border-radius: 50%; }
  .odoc-chip .name { font-size: 13px; font-weight: 500; }
  .odoc-chip.signin { padding: 7px 14px; background: var(--octo-primary); color: var(--octo-surface); font-weight: 600; }
  .odoc-chip.signin:hover { background: var(--octo-primary-hover); }

  /* Comment cards */
  #odoc-comment-layer { position: absolute; top: 0; left: 0; width: 100%; pointer-events: none; z-index: 999996; }
  .odoc-margin-comment { position: absolute; width: 280px; background: var(--octo-surface); border: 1px solid var(--octo-border); border-radius: var(--octo-radius-lg); padding: 12px; box-shadow: var(--octo-shadow-card); font: 13px system-ui, sans-serif; transition: box-shadow .15s, transform .15s, border-color .15s; z-index: 999996; pointer-events: auto; }
  .odoc-margin-comment:hover:not(.active) { box-shadow: var(--octo-shadow-menu); }
  .odoc-margin-comment.active { box-shadow: var(--octo-shadow-active); border-color: var(--octo-primary); }
  .odoc-margin-comment.odoc-unanchored { border-style: dashed; }
  .odoc-reanchor-btn { display: none; font-size: 10px; color: var(--octo-muted); text-transform: uppercase; letter-spacing: 0.05em; margin: 0 0 6px; cursor: pointer; background: none; border: none; padding: 0; text-align: left; }
  .odoc-margin-comment.odoc-unanchored .odoc-reanchor-btn { display: block; }
  /* Anchored cards also expose a "move anchor" action when they're active. */
  .odoc-margin-comment.active .odoc-reanchor-btn { display: block; }
  .odoc-reanchor-btn:hover { color: var(--octo-primary); }
  /* Label swap: "unanchored" wording on unanchored cards, "move anchor" on
     active anchored cards. */
  .odoc-reanchor-btn .odoc-reanchor-unanchored,
  .odoc-reanchor-btn .odoc-reanchor-anchored { display: none; }
  .odoc-margin-comment.odoc-unanchored .odoc-reanchor-btn .odoc-reanchor-unanchored { display: inline; }
  .odoc-margin-comment:not(.odoc-unanchored).active .odoc-reanchor-btn .odoc-reanchor-anchored { display: inline; }
  /* Container for the anchor action buttons. */
  .odoc-anchor-actions { display: flex; gap: 12px; align-items: center; margin: 0 0 6px; }
  /* While re-anchor mode is active, dim the rest of the UI and prompt the
     user to select. */
  /* Re-anchor banner: pinned below the bar with three actions. Visible
     only while body.odoc-reanchoring is set. */
  .odoc-reanchor-banner { display: none; position: fixed; top: 56px; left: 50%; transform: translateX(-50%); background: var(--octo-primary); color: var(--octo-surface); padding: 6px 10px 6px 14px; border-radius: var(--octo-radius-pill); font: 12px system-ui; z-index: 999999; align-items: center; gap: 6px; box-shadow: var(--octo-shadow-fab); }
  body.odoc-reanchoring .odoc-reanchor-banner { display: inline-flex; }
  .odoc-reanchor-banner .label { padding: 0 4px; }
  .odoc-reanchor-banner button { background: rgba(255,255,255,0.15); border: none; color: var(--octo-surface); padding: 4px 10px; border-radius: var(--octo-radius-pill); font: 12px system-ui; cursor: pointer; }
  .odoc-reanchor-banner button:hover { background: rgba(255,255,255,0.28); }
  .odoc-reanchor-banner button.danger { background: rgba(255,255,255,0.15); }
  .odoc-reanchor-banner button.danger:hover { background: var(--octo-danger); }
  /* Old-version strip — a thin, quiet bar just under the top bar shown when
     the viewer is on a non-latest version. Single-direction nudge: it only
     points forward to the latest version. Hidden by default; the bar-setup
     code reveals it (and adds the body padding) only when version < latest. */
  .odoc-oldver-strip { display: none; position: fixed; top: 49px; left: 0; right: 0; height: 28px; background: #fbf6e9; color: #6b5e3a; border-bottom: 1px solid #efe6cd; font: 12px system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; align-items: center; justify-content: center; gap: 6px; z-index: 999998; padding: 0 12px; }
  body.odoc-has-oldver-strip .odoc-oldver-strip { display: flex; }
  body.odoc-has-oldver-strip { padding-top: 77px !important; }
  .odoc-oldver-strip a { color: #8a6d1f; font-weight: 600; text-decoration: none; border-bottom: 1px solid currentColor; }
  .odoc-oldver-strip a:hover { color: #6b5413; }
  /* Ghost marker — a faint horizontal line at the unanchored comment's
     original Y position, so the user can see where the deleted text used
     to be. Stays in document coordinates. */
  .odoc-ghost-marker { position: absolute; left: 0; right: 320px; height: 0; border-top: 1px dashed #d4d4d4; pointer-events: none; z-index: 999990; }
  body.odoc-narrow .odoc-ghost-marker { display: none; }
  .odoc-margin-comment .author { display: flex; align-items: center; gap: 8px; margin-bottom: 6px; }
  .odoc-margin-comment .author img { width: 24px; height: 24px; border-radius: 50%; }
  .odoc-margin-comment .author .login { font-weight: 600; color: var(--octo-ink-strong); font-size: 13px; }
  .odoc-margin-comment .author .anon { color: var(--octo-muted); font-style: italic; }
  /* Agent identity — a simple "⚡ odoc-agent" badge in place of an avatar.
     The status chip on agent replies (applied / partial / question) lets
     the user tell at a glance whether their comment was addressed. */
  .odoc-agent-badge { display: inline-flex; width: 24px; height: 24px; border-radius: 50%; background: var(--octo-ink-strong); color: var(--octo-surface); align-items: center; justify-content: center; font-size: 13px; }
  .odoc-agent-reply { background: var(--octo-surface-subtle); border-left: 3px solid var(--octo-ink-strong); padding-left: 8px; }
  .odoc-agent-status { display: inline-block; font-size: 11px; padding: 1px 8px; border-radius: var(--octo-radius-pill); margin: 0 0 6px; font-weight: 600; }
  .odoc-agent-status-applied { background: var(--octo-ok-bg); color: var(--octo-ok-fg); }
  .odoc-agent-status-partial { background: var(--octo-warn-bg); color: var(--octo-warn-fg); }
  .odoc-agent-status-question { background: var(--octo-ask-bg); color: var(--octo-ask-fg); }
  .odoc-margin-comment .text { color: var(--octo-ink-strong); line-height: 1.45; word-wrap: break-word; }
  .odoc-margin-comment .meta { font-size: 11px; color: var(--octo-muted-dark); margin-top: 8px; display: flex; align-items: center; justify-content: space-between; gap: 8px; flex-wrap: wrap; }
  .odoc-margin-comment .meta > span:first-child { flex: 1 1 auto; min-width: 0; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .odoc-margin-comment .del { cursor: pointer; color: var(--octo-danger); }
  .odoc-margin-comment .del:hover { text-decoration: underline; }
  .odoc-margin-comment .actions { display: inline-flex; gap: 2px; align-items: center; flex-shrink: 0; }
  .odoc-margin-comment .copy-md { cursor: pointer; color: var(--octo-muted); display: inline-flex; align-items: center; padding: 3px; border-radius: var(--octo-radius-sm); transition: background .12s, color .12s; }
  .odoc-margin-comment .copy-md:hover { color: var(--octo-primary); background: var(--octo-surface-subtle); }
  .odoc-margin-comment .copy-md svg { width: 14px; height: 14px; display: block; }
  .odoc-margin-comment .odoc-reply-toggle { cursor: pointer; color: var(--octo-primary); font-weight: 500; padding: 2px 7px; border-radius: var(--octo-radius-sm); transition: background .12s; }
  .odoc-margin-comment .odoc-reply-toggle:hover { background: var(--octo-surface-subtle); }

  /* Reactions + emoji picker */
  .odoc-reactions { display: flex; flex-wrap: wrap; gap: 4px; margin-top: 6px; align-items: center; }
  .odoc-react-chip { position: relative; display: inline-flex; align-items: center; gap: 4px; font: 12px system-ui; background: var(--octo-surface-subtle); border: 1px solid var(--octo-border); border-radius: var(--octo-radius-pill); padding: 2px 8px; cursor: pointer; color: var(--octo-ink-soft); transition: background .12s, border-color .12s; }
  .odoc-react-chip:hover { background: var(--octo-surface-hover); }
  .odoc-react-chip.mine { background: var(--octo-mine-bg); border-color: var(--octo-primary); color: var(--octo-primary); }
  /* Agent reactions get a tinted background so users can scan a long doc
     and spot which comments the agent has already responded to. */
  .odoc-react-chip.agent { background: var(--octo-agent-bg); border-color: var(--octo-agent-border); color: var(--octo-agent-fg); }
  .odoc-react-chip.agent.mine { background: var(--octo-agent-bg); border-color: var(--octo-agent-border); color: var(--octo-agent-fg); }
  /* Custom reactors tooltip — shows the GitHub logins (or agent labels) of
     everyone who used this emoji. Native title= has ~1s delay; this is
     instant and styled to match the doc. */
  .odoc-react-chip[data-users]:hover::after {
    content: attr(data-users);
    position: absolute;
    bottom: calc(100% + 6px);
    left: 50%;
    transform: translateX(-50%);
    background: var(--octo-ink-strong);
    color: var(--octo-surface);
    padding: 4px 8px;
    border-radius: var(--octo-radius-md);
    font: 11px/1.3 system-ui;
    white-space: pre-wrap;
    max-width: 240px;
    pointer-events: none;
    z-index: 999999;
  }
  .odoc-react-add { background: transparent; border: none; color: var(--octo-faint); padding: 3px; border-radius: var(--octo-radius-sm); cursor: pointer; line-height: 1; transition: color .12s, opacity .12s, background .12s; display: inline-flex; align-items: center; }
  .odoc-react-add svg { width: 16px; height: 16px; display: block; }
  .odoc-reactions .odoc-react-add { opacity: 0; }
  .odoc-margin-comment:hover .odoc-reactions .odoc-react-add, .odoc-reply:hover .odoc-reactions .odoc-react-add, .odoc-reactions:has(.odoc-react-chip) .odoc-react-add { opacity: 1; }
  .odoc-react-add.inline svg { width: 14px; height: 14px; }
  .odoc-react-add.inline { opacity: 0.55; vertical-align: middle; }
  .odoc-react-add:hover { color: var(--octo-primary); opacity: 1; background: var(--octo-surface-subtle); }
  .odoc-emoji-picker { position: absolute; background: var(--octo-surface); border: 1px solid var(--octo-border); border-radius: var(--octo-radius-lg); padding: 6px; display: grid; grid-template-columns: repeat(6, 32px); gap: 2px; box-shadow: var(--octo-shadow-menu); z-index: 1000001; }
  .odoc-emoji-picker button { background: transparent; border: none; padding: 0; cursor: pointer; border-radius: var(--octo-radius-sm); width: 32px; height: 32px; font-size: 18px; line-height: 1; display: inline-flex; align-items: center; justify-content: center; }
  .odoc-emoji-picker button:hover { background: var(--octo-surface-subtle); }
  .odoc-emoji-picker button.odoc-emoji-text { grid-column: span 6; height: auto; padding: 6px 8px; font-size: 12px; font-weight: 600; color: var(--octo-primary); }
  .odoc-emoji-picker button.odoc-emoji-text:hover { background: var(--octo-mine-bg); }

  /* Replies + reply form */
  .odoc-replies-toggle { margin-top: 10px; padding-top: 10px; border-top: 1px dashed var(--octo-hairline); display: inline-flex; align-items: center; gap: 4px; cursor: pointer; font-size: 12px; color: var(--octo-primary); user-select: none; }
  .odoc-replies-toggle:hover { text-decoration: underline; }
  .odoc-replies-toggle .chev { transition: transform .15s; }
  .odoc-replies-toggle.open .chev { transform: rotate(90deg); }
  .odoc-replies { display: none; flex-direction: column; gap: 10px; margin-top: 10px; }
  .odoc-replies.open { display: flex; }
  .odoc-reply { padding-left: 12px; border-left: 2px solid var(--octo-border); }
  .odoc-reply .author { display: flex; align-items: center; gap: 6px; margin-bottom: 4px; }
  .odoc-reply .author img { width: 18px; height: 18px; border-radius: 50%; }
  .odoc-reply .author .login { font-weight: 600; font-size: 12px; color: var(--octo-ink-strong); }
  .odoc-reply .author .anon { color: var(--octo-muted); font-style: italic; font-size: 12px; }
  .odoc-reply .text { color: var(--octo-ink); font-size: 13px; line-height: 1.4; word-wrap: break-word; }
  .odoc-reply .meta { font-size: 11px; color: var(--octo-muted-dark); margin-top: 4px; display: flex; justify-content: space-between; }
  .odoc-reply .del { cursor: pointer; color: var(--octo-danger); }
  .odoc-reply .del:hover { text-decoration: underline; }
  .odoc-reply-form { display: none; margin-top: 10px; padding-top: 10px; border-top: 1px dashed var(--octo-hairline); }
  .odoc-reply-form.open { display: block; }
  .odoc-reply-form textarea { width: 100%; min-height: 48px; box-sizing: border-box; padding: 8px 10px; font: 13px system-ui; line-height: 1.45; color: var(--octo-ink); border: 1px solid var(--octo-border); border-radius: var(--octo-radius-md); resize: vertical; outline: none; transition: border-color .12s, box-shadow .12s; }
  .odoc-reply-form textarea::placeholder { color: var(--octo-muted); }
  .odoc-reply-form textarea:focus { border-color: var(--octo-primary); box-shadow: var(--octo-focus-ring); }
  .odoc-reply-form-foot { display: flex; justify-content: space-between; align-items: center; margin-top: 8px; }
  .odoc-reply-form-foot .hint { color: var(--octo-muted-dark); font-size: 11px; }
  .odoc-reply-form-foot .odoc-reply-submit { background: var(--octo-primary); color: var(--octo-surface); border: none; border-radius: var(--octo-radius-md); padding: 6px 14px; font: 12px system-ui; font-weight: 600; cursor: pointer; box-shadow: 0 1px 2px rgba(22,82,240,0.2); transition: background .12s, box-shadow .12s; }
  .odoc-reply-form-foot .odoc-reply-submit:hover { background: var(--octo-primary-hover); box-shadow: 0 2px 8px rgba(22,82,240,0.28); }

  /* Anchor highlights (Custom Highlight API + fallback span) */
  ::highlight(odoc-pending) { background-color: #fff3a8; }
  ::highlight(odoc-anchor) { background-color: var(--octo-highlight); }
  /* Active = clicked. Visibly different from resting: vivid yellow + thick
     gold underline. (The CSS Highlight API only supports background-color,
     color, and text-decoration — so we stack those.) */
  ::highlight(odoc-anchor-active) {
    background-color: var(--octo-highlight-active);
    text-decoration: underline solid #b8860b;
    text-decoration-thickness: 3px;
    text-underline-offset: 2px;
  }
  .odoc-anchor-mark { background: var(--octo-highlight); cursor: pointer; -webkit-box-decoration-break: clone; box-decoration-break: clone; }
  .odoc-anchor-mark:hover { background: #fdedb0; }
  .odoc-anchor-mark.active { background: var(--octo-highlight-active); box-shadow: 0 -3px 0 -1px #b8860b inset; }

  /* Element outlines + hover affordance */
  .odoc-element-outline { position: absolute; pointer-events: none; border: 1.5px solid rgba(22,82,240,0.35); border-radius: var(--octo-radius-sm); box-sizing: border-box; z-index: 999995; transition: border-color .15s, box-shadow .15s, border-width .15s; }
  .odoc-element-outline.pending { border-color: #f0d000; border-width: 2px; background: transparent; }
  .odoc-element-outline.active { border-color: var(--octo-primary); border-width: 2px; box-shadow: 0 0 0 4px rgba(22,82,240,0.18); }
  .odoc-hover-outline { position: absolute; pointer-events: none; z-index: 999995; border: 2px dashed var(--octo-primary); border-radius: var(--octo-radius-sm); background: rgba(22,82,240,0.06); box-sizing: border-box; transition: opacity .12s; }
  /* Clickable pill that appears NEXT TO commentable artifacts (img/canvas/svg/video/pre).
     Positioned just outside the artifact's right edge so it can't obscure
     content. Uses !important on the visible colors to defend against doc-side
     button:hover rules that would otherwise repaint our background. */
  /* Hover "Comment" pill. It floats over arbitrary user HTML, so every box
     property is reset with !important — otherwise the document's own rules (e.g.
     an "svg { margin: 16px; display: block }" doc template) leak in and inflate
     it. Compact, icon-forward, with a soft tinted shadow. */
  .odoc-comment-pill {
    position: absolute !important; z-index: 999998 !important;
    box-sizing: border-box !important; height: auto !important; width: auto !important;
    min-width: 0 !important; min-height: 0 !important; max-width: none !important;
    margin: 0 !important;
    background: var(--octo-primary) !important; color: #fff !important;
    font: 600 12px/1 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif !important;
    letter-spacing: 0.01em !important;
    padding: 6px 11px 6px 9px !important;
    border: none !important; border-radius: var(--octo-radius-pill) !important;
    cursor: pointer !important;
    box-shadow: 0 2px 8px rgba(22,82,240,0.32), 0 1px 2px rgba(0,0,0,0.12) !important;
    display: inline-flex !important; align-items: center !important; gap: 5px !important;
    transition: transform .12s ease, background-color .12s, box-shadow .12s, opacity .12s !important;
    text-decoration: none !important; white-space: nowrap !important;
    opacity: 1 !important; visibility: visible !important;
    -webkit-appearance: none !important; appearance: none !important;
  }
  .odoc-comment-pill:hover {
    background: var(--octo-primary-hover) !important; color: #fff !important;
    transform: translateY(-1px) !important;
    box-shadow: 0 4px 14px rgba(22,82,240,0.42), 0 1px 2px rgba(0,0,0,0.14) !important;
  }
  .odoc-comment-pill:active { transform: translateY(0) scale(0.97) !important; }
  .odoc-comment-pill svg {
    width: 13px !important; height: 13px !important; flex: 0 0 13px !important;
    margin: 0 !important; padding: 0 !important; display: block !important;
    vertical-align: middle !important; stroke: #fff !important; fill: none !important;
    max-width: none !important; box-shadow: none !important;
  }
  .odoc-drag-marquee { position: absolute; pointer-events: none; z-index: 999997; border: 1.5px solid var(--octo-primary); background: rgba(22,82,240,0.1); box-sizing: border-box; }

  /* Popup (new-comment) — light surface, consistent with the toolbar/cards. */
  .odoc-popup { position: absolute; background: var(--octo-surface); color: var(--octo-ink); border: 1px solid var(--octo-border); border-radius: var(--octo-radius-lg); padding: 14px; width: 320px; box-shadow: var(--octo-shadow-menu); z-index: 999998; font: 13px system-ui, sans-serif; }
  .odoc-popup .head { display: flex; justify-content: space-between; align-items: center; gap: 8px; margin-bottom: 10px; }
  /* Anchor preview as a subtle chip so it reads as "commenting on this". */
  .odoc-popup .head .h { flex: 1 1 auto; min-width: 0; background: var(--octo-surface-subtle); color: var(--octo-ink-soft); font-size: 12px; padding: 3px 8px; border-radius: var(--octo-radius-sm); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .odoc-popup .head .x { flex-shrink: 0; display: inline-flex; align-items: center; justify-content: center; width: 24px; height: 24px; cursor: pointer; color: var(--octo-muted); border-radius: var(--octo-radius-sm); font-size: 16px; line-height: 1; transition: background .12s, color .12s; }
  .odoc-popup .head .x:hover { background: var(--octo-surface-subtle); color: var(--octo-ink); }
  .odoc-popup textarea { width: 100%; min-height: 72px; background: var(--octo-surface); color: var(--octo-ink); border: 1px solid var(--octo-border); border-radius: var(--octo-radius-md); padding: 9px 10px; font: inherit; line-height: 1.45; resize: vertical; box-sizing: border-box; outline: none; transition: border-color .12s, box-shadow .12s; }
  .odoc-popup textarea::placeholder { color: var(--octo-muted); }
  .odoc-popup textarea:focus { border-color: var(--octo-primary); box-shadow: var(--octo-focus-ring); }
  .odoc-popup .foot { display: flex; justify-content: space-between; align-items: center; margin-top: 10px; }
  .odoc-popup .hint { color: var(--octo-muted-dark); font-size: 11px; }
  .odoc-popup .submit { background: var(--octo-primary); border: none; color: var(--octo-surface); padding: 7px 16px; border-radius: var(--octo-radius-md); cursor: pointer; font: inherit; font-weight: 600; box-shadow: 0 1px 2px rgba(22,82,240,0.2); transition: background .12s, box-shadow .12s; }
  .odoc-popup .submit:hover { background: var(--octo-primary-hover); box-shadow: 0 2px 8px rgba(22,82,240,0.28); }
  .odoc-popup .submit:disabled { opacity: 0.5; cursor: not-allowed; box-shadow: none; }
  .odoc-popup .signin-needed { color: var(--octo-warn-fg); font-size: 12px; padding: 8px 0; }

  /* Modal (sign-in) */
  .odoc-modal-bg { position: fixed; inset: 0; background: rgba(0,0,0,0.55); z-index: 1000000; display: flex; align-items: center; justify-content: center; font: 14px system-ui, sans-serif; }
  .odoc-modal { background: var(--octo-surface); color: var(--octo-ink-strong); border-radius: var(--octo-radius-xl); padding: 28px; width: 460px; box-shadow: 0 20px 60px rgba(0,0,0,0.3); }
  .odoc-modal h3 { margin: 0 0 8px; font-size: 20px; }
  .odoc-modal p { margin: 0 0 14px; color: var(--octo-ink-soft); line-height: 1.5; }
  .odoc-modal .code { background: var(--octo-ink-panel); color: var(--octo-surface); padding: 18px; border-radius: var(--octo-radius-lg); font: 24px ui-monospace, "SF Mono", Menlo, monospace; letter-spacing: 0.15em; text-align: center; margin: 0 0 14px; user-select: all; cursor: copy; }
  .odoc-modal .step { display: flex; gap: 10px; margin-bottom: 8px; color: var(--octo-ink-soft); }
  .odoc-modal .step .n { width: 22px; height: 22px; border-radius: 50%; background: var(--octo-primary); color: var(--octo-surface); display: inline-flex; align-items: center; justify-content: center; font-size: 12px; font-weight: 600; flex-shrink: 0; }
  .odoc-modal .actions { display: flex; gap: 8px; justify-content: flex-end; margin-top: 16px; }
  .odoc-modal button { padding: 8px 16px; border-radius: var(--octo-radius-md); font: inherit; cursor: pointer; border: 1px solid #ccc; background: var(--octo-surface); }
  .odoc-modal button.primary { background: var(--octo-primary); border-color: var(--octo-primary); color: var(--octo-surface); }
  .odoc-modal button.primary:hover { background: var(--octo-primary-hover); }
  .odoc-modal .status { color: var(--octo-muted); font-size: 13px; }
  /* Modal helper classes used by Publish/Share so dark-mode can override. */
  .odoc-modal .muted { color: var(--octo-muted-dark); font-size: 13px; }
  .odoc-modal .divider { border-top: 1px solid var(--octo-hairline); padding-top: 12px; margin-top: 12px; }
  .odoc-modal .danger { color: var(--octo-danger); font-size: 13px; }
  .odoc-modal code { background: var(--octo-surface-subtle); padding: 1px 5px; border-radius: 3px; }

  /* Bar collapse breakpoints — tied to viewport width, not layout class.
     The bar progressively hides elements as the viewport tightens, so it
     stays elegant at every size.
       ≥1100px: workspace · slug · v · | title | identity · share · ⋯
       <1100px: workspace ·          v · | title | identity · share · ⋯  (slug hides)
       < 900px: workspace ·          v · | title | avatar   · share · ⋯  (name hides)
       < 700px: workspace             · | title |            share · ⋯  (version+identity into ⋯) */
  @media (max-width: 1100px) {
    .odoc-bar .crumb-slug, .odoc-bar .crumb-sep-slug { display: none; }
  }
  @media (max-width: 900px) {
    .odoc-chip .name { display: none; }
    .odoc-chip { padding: 3px; }
    .odoc-bar #odoc-fork-btn, .odoc-bar #odoc-saveas-btn { display: none; }
    .odoc-bar .odoc-secondary-toggle { display: inline-flex; }
  }
  @media (max-width: 700px) {
    .odoc-bar { padding: 0 8px; gap: 4px; }
    .odoc-version-wrap { display: none; }
    .odoc-bar .doc-title { font-size: 13px; }
    .odoc-bar #odoc-copy-md-btn span { display: none; }
    .odoc-bar #odoc-publish-btn span, .odoc-bar #odoc-share-btn span { display: inline; }
  }

  /* Narrow mode (drawer + FAB) — still driven by the layout evaluator so
     it can also kick in when the comment column would crowd the article. */
  body.odoc-narrow #odoc-comment-layer { position: fixed; top: auto; left: 0; right: 0; bottom: 0; max-height: 70vh; width: 100%; pointer-events: auto; background: var(--octo-surface); border-top: 1px solid var(--octo-border); box-shadow: 0 -4px 24px rgba(0,0,0,0.08); transform: translateY(100%); transition: transform .2s; overflow-y: auto; padding: 12px 12px 24px; box-sizing: border-box; z-index: 999998; }
  body.odoc-narrow #odoc-comment-layer.open { transform: translateY(0); }
  body.odoc-narrow #odoc-comment-layer .odoc-drawer-handle { display: block; width: 36px; height: 4px; background: #ccc; border-radius: 2px; margin: 0 auto 12px; cursor: grab; touch-action: none; user-select: none; }
  body.odoc-narrow #odoc-comment-layer .odoc-drawer-handle:active { cursor: grabbing; }
  body.odoc-narrow .odoc-margin-comment { position: static !important; width: auto !important; left: auto !important; top: auto !important; margin-bottom: 10px; transform: none !important; }
  body.odoc-narrow .odoc-fab { position: fixed; bottom: 16px; right: 16px; z-index: 999997; background: var(--octo-primary); color: var(--octo-surface); border: none; border-radius: var(--octo-radius-pill); padding: 10px 16px; font: 13px system-ui; font-weight: 600; box-shadow: var(--octo-shadow-fab); cursor: pointer; display: inline-flex; align-items: center; gap: 6px; transition: transform .12s, box-shadow .12s; }
  body.odoc-narrow .odoc-fab:active { transform: scale(0.96); }
  /* Count badge inside the FAB — a contrasting pill so the number reads as a
     distinct chip rather than plain text next to the icon. */
  #odoc-fab-count { display: inline-flex; align-items: center; justify-content: center; min-width: 18px; height: 18px; padding: 0 5px; border-radius: var(--octo-radius-pill); background: var(--octo-surface); color: var(--octo-primary); font-size: 11px; font-weight: 700; line-height: 1; }
  body.odoc-narrow .odoc-popup { width: calc(100vw - 24px); max-width: 320px; left: 12px !important; }
  body.odoc-narrow .odoc-modal { width: calc(100vw - 32px); padding: 20px; }
  body.odoc-narrow .odoc-modal .code { font-size: 20px; }
  body.odoc-narrow .odoc-hover-outline, body.odoc-narrow .odoc-comment-pill, body.odoc-narrow .odoc-drag-marquee { display: none; }
  body.odoc-narrow .odoc-emoji-picker { grid-template-columns: repeat(6, 36px); }
  body.odoc-narrow .odoc-emoji-picker button { width: 36px; height: 36px; font-size: 20px; }
  @media (max-width: 480px) {
    .odoc-bar { padding: 0 10px; gap: 8px; }
    .odoc-bar button, .odoc-bar .odoc-menu-wrap > button { padding: 4px 8px; font-size: 12px; }
    .odoc-icon-btn span { display: none; }
    .odoc-emoji-picker { grid-template-columns: repeat(5, 40px); padding: 8px; }
    .odoc-emoji-picker button { width: 40px; height: 40px; font-size: 22px; }
    .odoc-emoji-picker button.odoc-emoji-text { grid-column: span 5; }
  }

  /* Footer */
  .odoc-footer { margin-top: 80px; padding: 20px 16px 28px; font: 12px system-ui, sans-serif; color: var(--octo-muted-dark); text-align: center; border-top: 1px solid var(--octo-hairline); box-sizing: border-box; max-width: 100%; }
  .odoc-footer .odoc-footer-row { display: inline-flex; flex-wrap: wrap; gap: 8px; align-items: center; justify-content: center; row-gap: 4px; }
  .odoc-footer a { color: var(--octo-muted-dark); text-decoration: none; }
  .odoc-footer a:hover { color: var(--octo-primary); text-decoration: underline; }
  .odoc-footer .sep { color: #ccc; }
  @media (max-width: 700px) { .odoc-footer .odoc-footer-row { flex-direction: column; gap: 4px; } .odoc-footer .sep { display: none; } }

  `;
  const style = document.createElement('style');
  style.textContent = css;
  document.head.appendChild(style);

  // ========== State ==========
  const state = {
    activeComments: [],            // last-fetched open comments
    cardEls: new Map(),            // id -> card element
    anchorMarks: new Map(),        // id -> { kind, el? (fallback span or outline), ranges? (Highlight API), targetEl? }
    activeId: null,
    narrow: false,
    reanchoringId: null,           // comment id awaiting a new selection for re-anchoring
  };

  // Highlight API: one shared registry for pending, one per saved comment.
  const pendingHighlight = HIGHLIGHT_API ? new Highlight() : null;
  if (HIGHLIGHT_API) {
    CSS.highlights.set('odoc-pending', pendingHighlight);
  }
  function rebuildSharedHighlights() {
    if (!HIGHLIGHT_API) return;
    const idle = new Highlight();
    const active = new Highlight();
    for (const [id, mark] of state.anchorMarks) {
      if (!mark.ranges) continue;
      const target = (id === state.activeId) ? active : idle;
      for (const r of mark.ranges) target.add(r);
    }
    CSS.highlights.set('odoc-anchor', idle);
    CSS.highlights.set('odoc-anchor-active', active);
  }
  function clearAllCommentHighlights() {
    if (!HIGHLIGHT_API) return;
    CSS.highlights.delete('odoc-anchor');
    CSS.highlights.delete('odoc-anchor-active');
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }

  // errMsg pulls the human message out of a /v1 error body
  // ({ error: { code, message } }), falling back to the status when the body
  // failed to parse (callers default it to {}).
  function errMsg(body, status) {
    return (body.error && body.error.message) || `HTTP ${status}`;
  }

  // ========== Top bar (HackMD-style three-group layout) ==========
  const bar = document.createElement('div');
  bar.className = 'odoc-bar';

  const versions = Array.isArray(cfg.versions) && cfg.versions.length ? cfg.versions : [{ n: version }];
  versions.sort((a, b) => (a.n || 0) - (b.n || 0));
  const slugCrumbLabel = isFork ? `fork of ${cfg.originalSlug || slug}` : slug;

  // Left group: workspace mark + slug crumb + version picker.
  const leftHtml = `
    <button class="odoc-bar-mark" id="odoc-bar-mark" title="octo-doc on GitHub" aria-label="octo-doc on GitHub"><svg width="24" height="24" viewBox="0 0 24 24" aria-hidden="true"><rect width="24" height="24" rx="6" fill="var(--octo-primary)"/><text x="12" y="16.5" text-anchor="middle" font-family="system-ui,-apple-system,sans-serif" font-size="11" font-weight="700" letter-spacing="-0.5" fill="#fff">o<tspan fill="#fff" fill-opacity="0.7">·</tspan>d</text></svg></button>
    <span class="crumb crumb-slug" title="${escapeHtml(slugCrumbLabel)}">${escapeHtml(slugCrumbLabel)}</span>
    <span class="crumb-sep crumb-sep-slug" aria-hidden="true">/</span>
    <div class="odoc-version-wrap">
      <button class="odoc-version-toggle" id="odoc-version-toggle" type="button" aria-haspopup="listbox" aria-expanded="false">v${version}${versions.length > 1 ? ' ▾' : ''}</button>
      ${versions.length > 1 ? `
        <div class="odoc-version-menu" id="odoc-version-menu" role="listbox">
          ${versions.map(v => `<button role="option" data-version="${v.n}" class="${v.n === version ? 'current' : ''}">v${v.n}${v.n === version ? ' · current' : ''}</button>`).join('')}
        </div>
      ` : ''}
    </div>`;

  // Center: doc title, pulled from the document's <title> at setup time (see
  // below). Starts EMPTY — the slug already shows in the left breadcrumb, so
  // echoing it here would just duplicate. A titled doc fills this in.
  const centerHtml = `<span class="doc-title" id="odoc-title"></span>`;

  // Right: copy menu + primary CTA (Share or Publish) + ⋯ overflow + identity.
  const copyMenuHtml = `
    <div class="odoc-menu-wrap">
      <button id="odoc-copy-md-btn" title="Copy as Markdown" aria-label="Copy as Markdown">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>
        <span>Copy</span>
      </button>
      <div class="odoc-menu" id="odoc-copy-md-menu">
        <button data-mode="doc">Doc only</button>
        <button data-mode="doc-comments">Doc + comments</button>
      </div>
    </div>`;

  // Primary CTA: published → Share (mint a code link); draft/local → Publish.
  const shareCtaHtml = `<button id="odoc-share-btn" class="primary" title="Share link" aria-label="Share">
         <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/></svg>
         <span>Share</span>
       </button>`;
  const publishCtaHtml = `<button id="odoc-publish-btn" class="primary" title="Publish to your octo-doc server" aria-label="Publish">
         <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 19V5"/><polyline points="5 12 12 5 19 12"/></svg>
         <span>Publish</span>
       </button>`;
  const primaryCtaHtml = isFork ? '' : (isPublished ? (isAuthor ? shareCtaHtml : '') : publishCtaHtml);

  // Fork / Save-as live in the ⋯ menu on narrow viewports.
  const forkBtnHtml = isPublished
    ? '<button id="odoc-fork-btn">Fork</button>'
    : (isFork ? '<button id="odoc-saveas-btn">Save As New Local Doc</button>' : '');

  const rightHtml = `
    ${copyMenuHtml}
    ${forkBtnHtml}
    ${primaryCtaHtml}
    <div class="odoc-menu-wrap">
      <button class="odoc-secondary-toggle" id="odoc-more-btn" aria-label="More" title="More">⋯</button>
      <div class="odoc-secondary-menu" id="odoc-secondary-menu">
        ${isPublished ? `${isAuthor ? '<button data-action="share">Share</button>' : ''}<button data-action="fork">Fork</button>` : ''}
        ${(isLocal || isDraft) ? '<button data-action="publish">Publish</button>' : ''}
        ${isFork ? '<button data-action="saveas">Save copy</button>' : ''}
        <button data-action="repo">octo-doc on GitHub</button>
      </div>
    </div>
    <span id="odoc-identity-slot"></span>`;

  bar.innerHTML = `
    <div class="odoc-bar-left">${leftHtml}</div>
    <div class="odoc-bar-center">${centerHtml}</div>
    <div class="odoc-bar-right">${rightHtml}</div>
  `;
  document.body.appendChild(bar);

  // Old-version strip — a quiet, single-direction nudge shown only when a
  // published viewer is looking at a non-latest version. `versions` is already
  // sorted ascending above, so the last entry is the latest. Fork/local modes
  // and the latest version itself get nothing.
  if (isPublished && versions.length > 1) {
    const latestVersion = versions[versions.length - 1].n;
    if (typeof version === 'number' && version < latestVersion) {
      const strip = document.createElement('div');
      strip.className = 'odoc-oldver-strip';
      const latestUrl = `/d/${encodeURIComponent(slug)}/v/${latestVersion}`;
      strip.innerHTML = `<span>You're viewing v${version} — the latest is <a href="${latestUrl}">v${latestVersion}</a></span>`;
      document.body.appendChild(strip);
      document.body.classList.add('odoc-has-oldver-strip');
    }
  }

  // Re-anchor banner — shown while a re-anchor action is in flight. Three
  // explicit actions to avoid the gesture conflict (clicking empty space
  // would otherwise be ambiguous with "deselect").
  const reanchorBanner = document.createElement('div');
  reanchorBanner.className = 'odoc-reanchor-banner';
  reanchorBanner.innerHTML = `
    <span class="label">Select text to move anchor</span>
    <button type="button" id="odoc-reanchor-remove">Remove anchor</button>
    <button type="button" id="odoc-reanchor-cancel" class="danger">Cancel</button>
  `;
  document.body.appendChild(reanchorBanner);

  const titleEl = document.querySelector('title');
  if (titleEl && titleEl.textContent) document.getElementById('odoc-title').textContent = titleEl.textContent;

  // Workspace mark in the bar's left → the open-source project. There is
  // no public catalog; the owner reaches their doc list via the profile
  // chip menu instead.
  document.getElementById('odoc-bar-mark').onclick = () =>
    window.open('https://github.com/lml2468/octo-doc', '_blank', 'noopener');

  // Fork: opens the renderable /fork view in a new tab AND triggers a download
  // (one click, both happen). We use a hidden iframe to fire the download so
  // the user keeps focus on the new fork tab.
  async function forkAndDownload() {
    // Fetch the fork HTML once, then both download AND open it via a blob URL.
    // This way the new tab shows exactly the SAME bytes the user has on disk —
    // a real local copy, not the worker-hosted /fork page. Self-contained:
    // closing the tab doesn't lose the file, and the tab has no worker
    // dependency (uses blob: not https:).
    const base = `/d/${encodeURIComponent(slug)}/v/${version}`;
    let bodyText;
    try {
      const resp = await fetch(`${base}/fork`);
      if (!resp.ok) throw new Error(`fork fetch failed: ${resp.status}`);
      bodyText = await resp.text();
    } catch (e) {
      // Fallback: old behavior (let the worker route handle download)
      window.location.href = `${base}/export?download=1`;
      return;
    }
    const blob = new Blob([bodyText], { type: 'text/html;charset=utf-8' });
    const blobUrl = URL.createObjectURL(blob);

    // 1. Trigger the file download via <a download>.
    const a = document.createElement('a');
    a.href = blobUrl;
    a.download = `${slug}-v${version}-fork.html`;
    document.body.appendChild(a);
    a.click();
    a.remove();

    // 2. Open the same blob in a new tab so the user sees their fork rendered.
    //    Small delay so the download starts before the new tab steals focus.
    setTimeout(() => {
      window.open(blobUrl, '_blank');
      // Revoke after a generous interval — the new tab may still be parsing.
      setTimeout(() => URL.revokeObjectURL(blobUrl), 60000);
    }, 250);
  }
  if (isPublished) {
    const fb = document.getElementById('odoc-fork-btn');
    if (fb) fb.onclick = forkAndDownload;
    const sb = document.getElementById('odoc-share-btn');
    if (sb) sb.onclick = (e) => { e.stopPropagation(); showShareModal(); };
  }
  if (isLocal) {
    const pb = document.getElementById('odoc-publish-btn');
    if (pb) pb.onclick = (e) => { e.stopPropagation(); showPublishModal(); };
  }
  if (isDraft) {
    const pb = document.getElementById('odoc-publish-btn');
    if (pb) pb.onclick = (e) => { e.stopPropagation(); showPromoteModal(); };
  }
  function triggerForkDownload(slug, version) {
    const a = document.createElement('a');
    a.href = `/d/${encodeURIComponent(slug)}/v/${version}/export?download=1`;
    a.download = `${slug}-v${version}-fork.html`;
    document.body.appendChild(a); a.click(); a.remove();
  }
  if (isFork) {
    // Save As: same download as Fork, but from within fork mode (no /fork open
    // since we ARE the fork tab already).
    const sa = document.getElementById('odoc-saveas-btn');
    if (sa) sa.onclick = () => triggerForkDownload(slug, version);
  }

  // Version picker — clicking a row navigates to /d/<slug>/v/<n>. The
  // worker handles version routing; we let the browser do the navigation
  // instead of any in-page swap so the user can hit Back to return.
  const versionToggle = document.getElementById('odoc-version-toggle');
  const versionMenu = document.getElementById('odoc-version-menu');
  if (versionToggle && versionMenu) {
    versionToggle.onclick = (e) => {
      e.stopPropagation();
      const open = versionMenu.classList.toggle('open');
      versionToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    };
    versionMenu.querySelectorAll('button').forEach(b => {
      b.onclick = (e) => {
        e.stopPropagation();
        versionMenu.classList.remove('open');
        const n = Number(b.dataset.version);
        if (!Number.isFinite(n) || n === version) return;
        location.href = `/d/${encodeURIComponent(slug)}/v/${n}`;
      };
    });
  }

  const copyBtn = document.getElementById('odoc-copy-md-btn');
  const copyMenu = document.getElementById('odoc-copy-md-menu');
  copyBtn.onclick = (e) => { e.stopPropagation(); copyMenu.classList.toggle('open'); };
  copyMenu.querySelectorAll('button').forEach(b => {
    b.onclick = async (e) => {
      e.stopPropagation();
      copyMenu.classList.remove('open');
      await window.__odocCopyDocMd(b.dataset.mode === 'doc-comments');
    };
  });

  const moreBtn = document.getElementById('odoc-more-btn');
  const secMenu = document.getElementById('odoc-secondary-menu');
  moreBtn.onclick = (e) => { e.stopPropagation(); secMenu.classList.toggle('open'); };
  secMenu.querySelectorAll('button').forEach(b => {
    b.onclick = (e) => {
      e.stopPropagation();
      secMenu.classList.remove('open');
      if (b.dataset.action === 'repo') window.open('https://github.com/lml2468/octo-doc', '_blank', 'noopener');
      if (b.dataset.action === 'fork') forkAndDownload();
      if (b.dataset.action === 'share') showShareModal();
      if (b.dataset.action === 'publish') { isDraft ? showPromoteModal() : showPublishModal(); }
      if (b.dataset.action === 'saveas') triggerForkDownload(slug, version);
    };
  });

  function renderIdentity() {
    const slot = document.getElementById('odoc-identity-slot');
    if (!isPublished) { slot.innerHTML = ''; return; }
    if (identity) {
      // Profile chip → dropdown. "My docs" is owner-only (the configured
      // ODOC_OWNER); everyone signed in still gets Sign out.
      slot.innerHTML =
        `<div class="odoc-menu-wrap">
          <button class="odoc-chip" id="odoc-me" aria-haspopup="menu" aria-expanded="false">
            <img src="${escapeHtml(identity.avatar_url || '')}" alt=""><span class="name">${escapeHtml(identity.login)}</span>
          </button>
          <div class="odoc-menu" id="odoc-me-menu" role="menu">
            ${isOwner ? `<button id="odoc-my-docs" role="menuitem">My docs</button>` : ''}
            <button id="odoc-signout" role="menuitem">Sign out</button>
          </div>
        </div>`;
      const meBtn = document.getElementById('odoc-me');
      const meMenu = document.getElementById('odoc-me-menu');
      meBtn.onclick = (e) => {
        e.stopPropagation();
        const open = meMenu.classList.toggle('open');
        meBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
      };
      if (isOwner) {
        document.getElementById('odoc-my-docs').onclick = () => {
          window.open('/me', '_blank', 'noopener');
        };
      }
      document.getElementById('odoc-signout').onclick = async () => {
        await fetch('/v1/auth/logout', { method: 'POST' });
        identity = null;
        isOwner = false;
        renderIdentity();
        refreshComments();
      };
    } else if (authConfigured) {
      slot.innerHTML = `<button class="odoc-chip signin" id="odoc-signin">Sign in</button>`;
      document.getElementById('odoc-signin').onclick = startLogin;
    } else {
      // No login provider configured — commenting is anonymous, no sign-in UI.
      slot.innerHTML = '';
    }
  }
  renderIdentity();

  // ========== Comment layer + FAB ==========
  const commentLayer = document.createElement('div');
  commentLayer.id = 'odoc-comment-layer';
  const drawerHandle = document.createElement('div');
  drawerHandle.className = 'odoc-drawer-handle';
  drawerHandle.setAttribute('aria-label', 'Drag down to close comments');
  commentLayer.appendChild(drawerHandle);
  document.body.appendChild(commentLayer);

  const fab = document.createElement('button');
  fab.className = 'odoc-fab';
  fab.style.display = 'none';
  fab.innerHTML = '💬 <span id="odoc-fab-count">0</span>';
  fab.onclick = (e) => { e.stopPropagation(); commentLayer.classList.toggle('open'); };
  document.body.appendChild(fab);

  // Drawer drag-to-close
  drawerHandle.onclick = (e) => { e.stopPropagation(); commentLayer.classList.remove('open'); };
  let drag = null;
  function dragStart(e) {
    e.preventDefault();
    drag = { y0: e.touches ? e.touches[0].clientY : e.clientY, dy: 0 };
    commentLayer.style.transition = 'none';
  }
  function dragMove(e) {
    if (!drag) return;
    const y = e.touches ? e.touches[0].clientY : e.clientY;
    drag.dy = Math.max(0, y - drag.y0);
    commentLayer.style.transform = `translateY(${drag.dy}px)`;
  }
  function dragEnd() {
    if (!drag) return;
    commentLayer.style.transition = '';
    commentLayer.style.transform = '';
    if (drag.dy > 40) commentLayer.classList.remove('open');
    drag = null;
  }
  drawerHandle.addEventListener('touchstart', dragStart, { passive: false });
  drawerHandle.addEventListener('touchmove', dragMove, { passive: true });
  drawerHandle.addEventListener('touchend', dragEnd);
  drawerHandle.addEventListener('mousedown', (e) => {
    dragStart(e);
    document.addEventListener('mousemove', dragMove);
    document.addEventListener('mouseup', function onUp() {
      dragEnd();
      document.removeEventListener('mousemove', dragMove);
      document.removeEventListener('mouseup', onUp);
    });
  });

  // ========== Footer ==========
  const footer = document.createElement('footer');
  footer.className = 'odoc-footer';
  footer.innerHTML =
    '<div class="odoc-footer-row">' +
      '<span>Powered by <a href="https://github.com/lml2468/octo-doc" target="_blank" rel="noopener">octo-doc</a></span>' +
    '</div>';
  document.body.appendChild(footer);

  // ========== Anchor matching (text → Range, element → Element) ==========
  // Flatten the document's commentable text into one string, plus a parallel
  // (node, offsetInString) map. Selections often span multiple text nodes
  // (e.g. across <b>, <a>, <em>), so a per-node indexOf would miss them.
  // Searching the flattened string handles that uniformly.
  // Build a flat view of the document's commentable text plus a per-text-node
  // offset map. We also build a *normalized* projection where every run of
  // whitespace collapses to a single space. Multi-paragraph selections — which
  // `Selection.toString()` returns with embedded "\n\n" — match against the
  // normalized projection; the projection→raw map lets us recover the exact
  // text-node/offset pair for the Range.
  function collectTextNodes() {
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT, {
      acceptNode(n) {
        if (!n.parentElement) return NodeFilter.FILTER_REJECT;
        if (n.parentElement.closest(UI_TEXT)) return NodeFilter.FILTER_REJECT;
        // Skip script/style/template etc — their .textContent is irrelevant.
        const tag = n.parentElement.tagName;
        if (tag === 'SCRIPT' || tag === 'STYLE' || tag === 'TEMPLATE' || tag === 'NOSCRIPT') return NodeFilter.FILTER_REJECT;
        return NodeFilter.FILTER_ACCEPT;
      }
    });
    const nodes = [];
    let total = '';
    // norm[i] = raw-string offset corresponding to normalized-string offset i.
    let norm = '';
    const normToRaw = [];
    let prevWasSpace = false;
    while (walker.nextNode()) {
      const n = walker.currentNode;
      const start = total.length;
      const v = n.nodeValue;
      nodes.push({ node: n, start, end: start + v.length });
      total += v;
      // If the previous block ended on non-space content and the next text
      // node lives under a different block-level parent, treat the boundary
      // as a single space in the normalized projection. This is what makes
      // "para1\n\npara2" (from Selection.toString) collapse to "para1 para2".
      for (let i = 0; i < v.length; i++) {
        const ch = v.charCodeAt(i);
        const isWs = ch === 0x20 || ch === 0x09 || ch === 0x0a || ch === 0x0d || ch === 0xa0;
        if (isWs) {
          if (!prevWasSpace && norm.length) {
            norm += ' ';
            normToRaw.push(start + i);
            prevWasSpace = true;
          }
        } else {
          norm += v[i];
          normToRaw.push(start + i);
          prevWasSpace = false;
        }
      }
    }
    // Sentinel: normToRaw.length === norm.length, plus one trailing entry so
    // end offsets at the very end of the doc still translate.
    normToRaw.push(total.length);
    return { nodes, total, norm, normToRaw };
  }
  // Collapse runs of whitespace into a single space so saved anchor text
  // and the doc's normalized projection agree on inter-block boundaries.
  // Two flavors:
  //   normalizeNeedle: also trims edges. The user's selection often has
  //     a stray leading/trailing newline that's not present in the doc
  //     text we want to match against.
  //   normalizeContext: preserves leading/trailing whitespace. Boundary
  //     whitespace is what makes context disambiguation work — the doc's
  //     normalized projection has a single space between block elements
  //     before the needle, so trimming context tails would strand them at
  //     punctuation and break commonSuffixLen.
  function normalizeNeedle(s) {
    return s ? s.replace(/\s+/g, ' ').trim() : '';
  }
  function normalizeContext(s) {
    return s ? s.replace(/\s+/g, ' ') : '';
  }
  // Back-compat alias for older callers (getContext etc.) — they handle
  // their own normalization where needed.
  function normalizeQuery(s) { return normalizeNeedle(s); }
  // Locate (node, offset) in the per-node map from a raw-string offset.
  function locateAt(nodes, rawOffset) {
    let lo = 0, hi = nodes.length - 1;
    while (lo <= hi) {
      const mid = (lo + hi) >> 1;
      const n = nodes[mid];
      if (rawOffset < n.start) hi = mid - 1;
      else if (rawOffset > n.end) lo = mid + 1;
      else return { node: n.node, offset: rawOffset - n.start };
    }
    return null;
  }
  // Anchor matching protocol (architectural):
  //
  //   Invariant: a text anchor resolves only when the saved context_before /
  //   context_after agrees with the candidate location. The same `text` may
  //   appear N times in the doc; context is the disambiguator that picks
  //   THIS occurrence — moving the anchor (re-anchor) rewrites the context
  //   to the new neighbors, so the matcher MUST refuse to fall back to the
  //   first hit when context fails to match. Without this guard, re-anchor
  //   silently re-resolves to the old location whenever the old text still
  //   exists in the doc (the "stale highlight" bug).
  //
  //   We compare longer context windows (60 chars by default, scaled down
  //   to what was saved) for stronger disambiguation, and require at least
  //   one side to match to accept the hit. If no candidate clears the bar,
  //   return null and let the caller fall back to the saved position ratio.
  const CTX_MATCH_LEN = 60;
  function findTextRange(anchor, cache) {
    if (!anchor || !anchor.text || anchor.text.length < 2) return null;
    const view = cache || collectTextNodes();
    if (!view.norm) return null;

    const needleN = normalizeNeedle(anchor.text);
    if (needleN.length < 2) return null;
    const beforeN = normalizeContext(anchor.context_before);
    const afterN = normalizeContext(anchor.context_after);

    const hits = [];
    for (let i = 0; (i = view.norm.indexOf(needleN, i)) !== -1; i += Math.max(1, needleN.length)) {
      hits.push(i);
      if (hits.length > 64) break;
    }
    if (!hits.length) return null;

    // Single hit and no saved context → unambiguous, accept.
    // Multiple hits with no context → ambiguous, refuse.
    const hasContext = beforeN.length > 0 || afterN.length > 0;
    if (hits.length === 1 && !hasContext) {
      return rangeFromNormalizedOffsets(view, hits[0], needleN.length);
    }
    if (!hasContext) return null;

    // Score each hit by how many context chars match on each side. Require
    // a *meaningful* match — at least MIN_CTX_MATCH chars — so we don't
    // accept hits that only agree on trailing punctuation/spaces ("." or
    // ": "). That guard is what makes re-anchor robust: when the user
    // moves the anchor, the new context_before/after refer to the new
    // neighbors; the old location's punctuation overlap shouldn't be
    // enough to keep the highlight there.
    const MIN_CTX_MATCH = 4;
    const ctxLen = CTX_MATCH_LEN;
    const bTail = beforeN.slice(-Math.min(ctxLen, beforeN.length));
    const aHead = afterN.slice(0, Math.min(ctxLen, afterN.length));
    let bestIdx = -1, bestScore = 0;
    for (const h of hits) {
      const beforeSlice = view.norm.slice(Math.max(0, h - ctxLen), h);
      const afterSlice = view.norm.slice(h + needleN.length, h + needleN.length + ctxLen);
      const bScore = commonSuffixLen(beforeSlice, bTail);
      const aScore = commonPrefixLen(afterSlice, aHead);
      // A side counts only if it cleared the meaningful-match bar.
      const score = (bScore >= MIN_CTX_MATCH ? bScore : 0) + (aScore >= MIN_CTX_MATCH ? aScore : 0);
      if (score > bestScore) { bestScore = score; bestIdx = h; }
    }
    // Reject if no candidate cleared the meaningful-match bar. Caller will
    // use the saved fallback ratio rather than highlight the wrong spot.
    if (bestIdx === -1 || bestScore === 0) return null;

    return rangeFromNormalizedOffsets(view, bestIdx, needleN.length);
  }
  function rangeFromNormalizedOffsets(view, normIdx, normLen) {
    const rawStart = view.normToRaw[normIdx];
    const rawEnd = view.normToRaw[normIdx + normLen] ?? view.total.length;
    const startLoc = locateAt(view.nodes, rawStart);
    const endLoc = locateAt(view.nodes, rawEnd);
    if (!startLoc || !endLoc) return null;
    const range = document.createRange();
    try {
      range.setStart(startLoc.node, startLoc.offset);
      range.setEnd(endLoc.node, endLoc.offset);
    } catch { return null; }
    return range;
  }
  function commonSuffixLen(a, b) {
    let i = 0;
    const min = Math.min(a.length, b.length);
    while (i < min && a[a.length - 1 - i] === b[b.length - 1 - i]) i++;
    return i;
  }
  function commonPrefixLen(a, b) {
    let i = 0;
    const min = Math.min(a.length, b.length);
    while (i < min && a[i] === b[i]) i++;
    return i;
  }
  function findElement(anchor) {
    if (!anchor) return null;
    // Server-side reconciliation may have marked the anchor as lost — the
    // artifact is gone in this version. Render unanchored, never guess.
    if (anchor.kind === 'lost') return null;

    // 1. IDENTITY-FIRST: anchor.aid is the artifact's content-derived id
    //    stamped by the worker. Same artifact across versions = same aid
    //    iff its content didn't change. When content DID change between
    //    versions, the worker mints a new aid in the new version AND
    //    keeps the old aid in `anchor.aid_history` (newest first) so that
    //    viewers of OLDER versions still resolve to the same comment.
    const aidCandidates = [];
    if (anchor.aid) aidCandidates.push(anchor.aid);
    if (Array.isArray(anchor.aid_history)) {
      for (const x of anchor.aid_history) if (x && !aidCandidates.includes(x)) aidCandidates.push(x);
    }
    const fromSelector = anchor.selector && (/\[data-odoc-aid="([^"]+)"\]/.exec(anchor.selector) || [])[1];
    if (fromSelector && !aidCandidates.includes(fromSelector)) aidCandidates.push(fromSelector);
    if (aidCandidates.length) {
      for (const aid of aidCandidates) {
        const byAid = document.querySelector(`[data-odoc-aid="${aid}"]`);
        if (byAid) return byAid;
      }
      // Recorded aid(s), none present in this DOM → unanchored, never fallback.
      return null;
    }

    // 2. LEGACY PATH (pre-aid comments): try the stored selector, but
    //    NEVER trust the result without fingerprint validation. A bare
    //    positional selector can silently point at a different artifact.
    let bySelector = null;
    if (anchor.selector) {
      try { bySelector = document.querySelector(anchor.selector); } catch { bySelector = null; }
    }
    const fp = anchor.fingerprint;

    // 2a. Has fingerprint: trust selector ONLY if it matches the fp,
    //     otherwise scan all candidates.
    if (fp) {
      if (bySelector && fingerprintScore(fp, elementFingerprint(bySelector)) >= 0.6) {
        return bySelector;
      }
      let best = null, bestScore = 0;
      const tag = fp.tag || '*';
      let cands;
      try { cands = document.querySelectorAll(tag); } catch { cands = []; }
      cands.forEach(el => {
        if (el.closest && el.closest(UI_ALL)) return;
        const sc = fingerprintScore(fp, elementFingerprint(el));
        if (sc > bestScore) { bestScore = sc; best = el; }
      });
      if (best && bestScore >= 0.6) return best;
      // No confident match → unanchored, never the wrong artifact.
      return null;
    }

    // 2b. No fingerprint AND no aid (truly legacy). Validate the selector
    //     match against the stored `label` (the artifact's tag). If the
    //     tag matches, accept it — but this path is fragile and the
    //     server-side reconciliation should convert these to aid anchors
    //     on the next upload, after which we never hit this branch again.
    if (bySelector && (!anchor.label || bySelector.tagName.toLowerCase() === anchor.label.toLowerCase())) {
      return bySelector;
    }
    return null;
  }

  // Fallback span path — only used when CSS.highlights is unavailable AND the
  // range is single-text-node (no cross-element risk → no empty bars).
  function fallbackWrapAsSpan(comment, range) {
    if (range.startContainer !== range.endContainer || range.startContainer.nodeType !== Node.TEXT_NODE) return null;
    const mark = document.createElement('span');
    mark.className = 'odoc-anchor-mark';
    mark.dataset.commentId = comment.id;
    try { range.surroundContents(mark); return mark; } catch { return null; }
  }
  function unwrapFallbackSpans() {
    document.querySelectorAll('.odoc-anchor-mark').forEach(mark => {
      const parent = mark.parentNode;
      if (!parent) return;
      while (mark.firstChild) parent.insertBefore(mark.firstChild, mark);
      parent.removeChild(mark);
      parent.normalize?.();
    });
  }

  // ========== Reactions + comment cards ==========
  const QUICK_EMOJIS = ['👍', '❤️', '🔥', '🎉', '😂', '🤔', '👀', '🚀', '✅', '❌', '❓', '❗'];
  const QUICK_TEXT_REACTIONS = ['LGTM'];
  const REACT_ICON_SVG = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M8 14s1.5 2 4 2 4-2 4-2"/><line x1="9" y1="9" x2="9.01" y2="9"/><line x1="15" y1="9" x2="15.01" y2="9"/><line x1="19" y1="6" x2="19" y2="10"/><line x1="21" y1="8" x2="17" y2="8"/></svg>`;

  function renderAuthor(author) {
    if (!author) return `<div class="author"><span class="anon">anonymous</span></div>`;
    if (author.kind === 'agent') {
      // Agent identity (currently always 'odoc-agent'). No avatar URL — use
      // a generic icon-circle to differentiate from human commenters.
      return `<div class="author odoc-agent-author"><span class="odoc-agent-badge">⚡</span><span class="login">${escapeHtml(author.login || 'odoc-agent')}</span></div>`;
    }
    const avatar = author.avatar_url ? `<img src="${escapeHtml(author.avatar_url)}" alt="">` : '';
    return `<div class="author">${avatar}<span class="login">${escapeHtml(author.login || 'anonymous')}</span></div>`;
  }
  function renderReactionsRow(target) {
    const reactions = target.reactions || {};
    const me = identity?.login || 'anon';
    const entries = Object.entries(reactions).filter(([, u]) => u && u.length > 0);
    if (!entries.length) return '';
    const chips = entries.map(([emoji, users]) => {
      const mine = users.includes(me);
      const hasAgent = users.includes('odoc-agent');
      const cls = [`odoc-react-chip`, mine ? 'mine' : '', hasAgent ? 'agent' : ''].filter(Boolean).join(' ');
      return `<span class="${cls}" data-emoji="${escapeHtml(emoji)}" data-target-id="${escapeHtml(target.id)}" data-users="${users.map(escapeHtml).join('\n')}">${escapeHtml(emoji)} ${users.length}</span>`;
    }).join('');
    return `<div class="odoc-reactions" data-target-id="${escapeHtml(target.id)}">${chips}<button class="odoc-react-add" data-target-id="${escapeHtml(target.id)}" title="Add reaction" aria-label="Add reaction">${REACT_ICON_SVG}</button></div>`;
  }
  function renderReactInline(target) {
    return `<button class="odoc-react-add inline" data-target-id="${escapeHtml(target.id)}" title="Add reaction" aria-label="Add reaction">${REACT_ICON_SVG}</button>`;
  }
  function renderReply(reply) {
    const canDelete = !isFork && (!isPublished || (identity && reply.author && identity.login === reply.author.login));
    const hasReactions = reply.reactions && Object.values(reply.reactions).some(u => u && u.length > 0);
    const isAgent = reply.author?.kind === 'agent';
    // Whitelist the status (it drives a CSS class) instead of interpolating raw.
    const safeStatus = ['applied', 'partial', 'question'].includes(reply.agent_status) ? reply.agent_status : null;
    const statusChip = safeStatus
      ? `<span class="odoc-agent-status odoc-agent-status-${safeStatus}">${
          safeStatus === 'applied' ? '✓ applied' :
          safeStatus === 'partial' ? '◐ partial' :
          '? question'
        }</span>`
      : '';
    return `<div class="odoc-reply${isAgent ? ' odoc-agent-reply' : ''}" data-comment-id="${escapeHtml(reply.id)}">
      ${renderAuthor(reply.author)}
      ${statusChip}
      <div class="text">${escapeHtml(reply.text)}</div>
      ${hasReactions ? renderReactionsRow(reply) : ''}
      <div class="meta">
        <span>${new Date(reply.created_at).toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })}</span>
        <span class="actions">
          ${!hasReactions && !isFork ? renderReactInline(reply) : ''}
          ${canDelete ? `<span class="del" data-id="${escapeHtml(reply.id)}">delete</span>` : ''}
        </span>
      </div>
    </div>`;
  }
  function buildCard(comment) {
    const card = document.createElement('div');
    card.className = 'odoc-margin-comment';
    card.dataset.commentId = comment.id;
    const canDelete = !isFork && (!isPublished || (identity && comment.author && identity.login === comment.author.login));
    const replies = Array.isArray(comment.replies) ? comment.replies : [];
    const hasReactions = comment.reactions && Object.values(comment.reactions).some(u => u && u.length > 0);
    card.innerHTML = `
      ${isFork ? '' : `<div class="odoc-anchor-actions">
        <button class="odoc-reanchor-btn" type="button" data-id="${escapeHtml(comment.id)}"><span class="odoc-reanchor-unanchored">unanchored — click to re-anchor</span><span class="odoc-reanchor-anchored">↻ move anchor</span></button>
      </div>`}
      ${renderAuthor(comment.author)}
      <div class="text">${escapeHtml(comment.text)}</div>
      ${hasReactions ? renderReactionsRow(comment) : ''}
      <div class="meta">
        <span>v${comment.version} · ${new Date(comment.created_at).toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })}</span>
        <span class="actions">
          ${!hasReactions && !isFork ? renderReactInline(comment) : ''}
          ${isFork ? '' : `<span class="odoc-reply-toggle" data-id="${escapeHtml(comment.id)}">Reply</span>`}
          <span class="copy-md" data-id="${escapeHtml(comment.id)}" title="Copy as Markdown" aria-label="Copy as Markdown"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg></span>
          ${canDelete ? `<span class="del" data-id="${escapeHtml(comment.id)}">delete</span>` : ''}
        </span>
      </div>
      ${replies.length ? `
        <div class="odoc-replies-toggle" data-id="${escapeHtml(comment.id)}">
          <svg class="chev" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="9 18 15 12 9 6"/></svg>
          ${replies.length} ${replies.length === 1 ? 'reply' : 'replies'}
        </div>
        <div class="odoc-replies">${replies.map(r => renderReply(r)).join('')}</div>
      ` : ''}
      ${isFork ? '' : `<div class="odoc-reply-form" data-parent-id="${escapeHtml(comment.id)}">
        <textarea placeholder="Reply…"></textarea>
        <div class="odoc-reply-form-foot">
          <span class="hint">⌘+Enter to submit · Esc to cancel</span>
          <button class="odoc-reply-submit">Reply</button>
        </div>
      </div>`}
    `;

    const repliesToggle = card.querySelector('.odoc-replies-toggle');
    const repliesEl = card.querySelector('.odoc-replies');
    if (repliesToggle && repliesEl) {
      repliesToggle.onclick = (e) => {
        e.stopPropagation();
        const open = repliesEl.classList.toggle('open');
        repliesToggle.classList.toggle('open', open);
        requestAnimationFrame(repositionCards);
      };
    }

    const copyMdBtn = card.querySelector('.copy-md');
    if (copyMdBtn) copyMdBtn.onclick = (e) => { e.stopPropagation(); window.__odocCopyCommentMd(comment.id, copyMdBtn); };

    const reBtn = card.querySelector('.odoc-reanchor-btn');
    if (reBtn) reBtn.onclick = (e) => { e.stopPropagation(); startReanchor(comment.id); };

    card.querySelectorAll('.del').forEach(del => {
      del.onclick = async (e) => {
        e.stopPropagation();
        const r = await fetch(`/v1/comments?slug=${encodeURIComponent(slug)}&id=${del.dataset.id}&version=${version}`, { method: 'DELETE' });
        if (!r.ok) {
          // Surface the failure instead of silently re-rendering the comment.
          const err = await r.json().catch(() => ({}));
          alert('Could not delete: ' + errMsg(err, r.status));
          return;
        }
        // Belt + suspenders: drop the active highlight before refresh in case
        // the deleted comment was the active one (which would leave a stale
        // ::highlight(odoc-anchor-active) ring until refresh completes).
        setActiveComment(null);
        await refreshComments();
      };
    });

    const replyToggle = card.querySelector('.odoc-reply-toggle');
    const replyForm = card.querySelector('.odoc-reply-form');
    if (replyToggle && replyForm) {
      replyToggle.onclick = (e) => {
        e.stopPropagation();
        if (authConfigured && !identity) { startLogin(); return; }
        replyForm.classList.toggle('open');
        if (replyForm.classList.contains('open')) {
          replyForm.querySelector('textarea').focus();
          requestAnimationFrame(repositionCards);
        }
      };
      const replyTa = replyForm.querySelector('textarea');
      const submitReply = async () => {
        const text = replyTa.value.trim();
        if (!text) return;
        const r = await fetch('/v1/comments', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ slug, parent_id: comment.id, text, version })
        });
        if (r.status === 401) { startLogin(); return; }
        replyTa.value = '';
        replyForm.classList.remove('open');
        await refreshComments();
      };
      replyForm.querySelector('.odoc-reply-submit').onclick = (e) => { e.stopPropagation(); submitReply(); };
      replyTa.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); submitReply(); }
        if (e.key === 'Escape') { replyForm.classList.remove('open'); requestAnimationFrame(repositionCards); }
      });
    }

    card.querySelectorAll('.odoc-react-chip').forEach(chip => {
      chip.onclick = async (e) => {
        e.stopPropagation();
        if (isFork) return; // read-only mode
        if (authConfigured && !identity) { startLogin(); return; }
        await fetch('/v1/reactions', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ slug, comment_id: chip.dataset.targetId, emoji: chip.dataset.emoji, version })
        });
        await refreshComments();
      };
    });
    card.querySelectorAll('.odoc-react-add').forEach(addBtn => {
      addBtn.onclick = (e) => {
        e.stopPropagation();
        if (authConfigured && !identity) { startLogin(); return; }
        openEmojiPicker(addBtn, addBtn.dataset.targetId);
      };
    });

    card.addEventListener('click', (e) => { e.stopPropagation(); setActiveComment(comment.id); });
    return card;
  }

  // ========== Emoji picker ==========
  let emojiPicker = null;
  function closeEmojiPicker() { if (emojiPicker) { emojiPicker.remove(); emojiPicker = null; } }
  function openEmojiPicker(anchorBtn, targetId) {
    closeEmojiPicker();
    emojiPicker = document.createElement('div');
    emojiPicker.className = 'odoc-emoji-picker';
    emojiPicker.innerHTML =
      QUICK_EMOJIS.map(e => `<button data-emoji="${e}">${e}</button>`).join('') +
      QUICK_TEXT_REACTIONS.map(t => `<button class="odoc-emoji-text" data-emoji="${t}">${t}</button>`).join('');
    document.body.appendChild(emojiPicker);
    const r = anchorBtn.getBoundingClientRect();
    emojiPicker.style.visibility = 'hidden';
    emojiPicker.style.top = '0'; emojiPicker.style.left = '0';
    const pw = emojiPicker.offsetWidth, ph = emojiPicker.offsetHeight;
    let left = window.scrollX + r.left;
    let top = window.scrollY + r.bottom + 6;
    const vpRight = window.scrollX + window.innerWidth - 8;
    if (left + pw > vpRight) left = Math.max(8, (window.scrollX + r.right) - pw);
    const vpBottom = window.scrollY + window.innerHeight - 8;
    if (top + ph > vpBottom) top = window.scrollY + r.top - ph - 6;
    emojiPicker.style.top = top + 'px'; emojiPicker.style.left = left + 'px';
    emojiPicker.style.visibility = '';
    emojiPicker.querySelectorAll('button').forEach(b => {
      b.onclick = async (e) => {
        e.stopPropagation();
        const emoji = b.dataset.emoji;
        closeEmojiPicker();
        await fetch('/v1/reactions', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ slug, comment_id: targetId, emoji, version })
        });
        await refreshComments();
      };
    });
  }

  // ========== Card positioning + active state ==========
  // Single source of truth for "where does the article column live?".
  // Returns viewport-coord metrics for the widest non-UI container element.
  // Caller can add window.scrollX to `right`/`left` for page coords.
  const ARTICLE_EXCLUDE = UI_CONTAINERS;
  function getArticleMetrics() {
    const candidates = document.querySelectorAll('main, article, .wrap, .content, .container');
    let best = null, bestRect = null, bestW = 0;
    for (const el of candidates) {
      if (el.closest(ARTICLE_EXCLUDE)) continue;
      const r = el.getBoundingClientRect();
      if (r.width > bestW && r.width > 200 && r.width < window.innerWidth) {
        best = el; bestRect = r; bestW = r.width;
      }
    }
    if (best) {
      return { el: best, width: bestRect.width, right: bestRect.right, left: bestRect.left };
    }
    // Fallback: pick the widest prose-ish element so margin cards have somewhere
    // to anchor on pages with no wrapping container.
    let fbRight = 0, fbLeft = 0, fbW = 0;
    for (const el of document.querySelectorAll('p, h1, h2, h3')) {
      if (el.closest(ARTICLE_EXCLUDE)) continue;
      const r = el.getBoundingClientRect();
      if (r.width > fbW && r.width > 300 && r.width < window.innerWidth) {
        fbW = r.width; fbRight = r.right; fbLeft = r.left;
      }
    }
    if (fbW > 0) {
      return { el: document.body, width: fbW, right: fbRight, left: fbLeft };
    }
    return { el: document.body, width: Infinity, right: 0, left: 0 };
  }

  function repositionCards() {
    // Always reposition element outlines first — they should track their
    // anchor element on every layout change regardless of narrow/wide mode.
    document.querySelectorAll('.odoc-element-outline:not(.pending)').forEach(o => o._reposition?.());
    if (state.narrow) {
      for (const card of state.cardEls.values()) { card.style.top = ''; card.style.left = ''; }
      return;
    }
    const margin = 12, cardGap = 16, cardWidth = 280;
    const metrics = getArticleMetrics();
    const rightEdge = metrics.width > 0 && metrics.right > 0
      ? metrics.right + window.scrollX
      : window.innerWidth - 320;
    let cardLeft = rightEdge + cardGap;
    const maxLeft = window.scrollX + window.innerWidth - cardWidth - 12;
    if (cardLeft > maxLeft) cardLeft = maxLeft;

    // Unified layout: every card (anchored + unanchored with fallback) is
    // placed in a single Y-sorted pass. This eliminates the inter-group
    // overlap class — previously anchored cards and unanchored-with-fallback
    // cards used independent prevBottoms and could land on top of each
    // other when their Ys interleaved.
    //
    // Cards without a fallback ratio (legacy comments) park below the
    // article in stable id order — their Y depends only on themselves and
    // the article height, so adding/removing other cards doesn't ripple.
    const articleEl = metrics.el || document.body;
    const articleTop = articleEl.getBoundingClientRect().top + window.scrollY;
    const articleHeight = Math.max(1, articleEl.scrollHeight);

    const rows = [];
    for (const c of state.activeComments) {
      const card = state.cardEls.get(c.id);
      if (!card) continue;
      const mark = state.anchorMarks.get(c.id);
      if (mark && (mark.ranges?.[0] || mark.el)) {
        // Anchored: place at its anchor's vertical position.
        const r = (mark.ranges?.[0] || mark.el).getBoundingClientRect();
        rows.push({ card, c, y: r.top + window.scrollY, anchored: true });
      } else if (c.anchor?.fallback && typeof c.anchor.fallback.ratio === 'number') {
        // Unanchored with saved fallback: place at the original ratio.
        rows.push({ card, c, y: articleTop + c.anchor.fallback.ratio * articleHeight, anchored: false });
      }
    }
    rows.sort((a, b) => a.y - b.y);

    let prevBottom = 0;
    for (const row of rows) {
      let y = row.y;
      if (y < prevBottom + margin) y = prevBottom + margin;
      row.card.style.top = y + 'px';
      row.card.style.left = cardLeft + 'px';
      if (row.anchored) row.card.classList.remove('odoc-unanchored');
      else {
        row.card.classList.add('odoc-unanchored');
        // Ghost marker shows where the deleted text USED to be — only
        // meaningful when the anchor was lost involuntarily (the doc was
        // rewritten). When the user explicitly removed the anchor via the
        // "Remove anchor" pill, we set kind:'none' and shouldn't render a
        // ghost at all (they intentionally cleared it).
        if (row.c.anchor?.kind !== 'none') {
          renderGhostMarker(row.c.id, articleTop + row.c.anchor.fallback.ratio * articleHeight);
        } else {
          removeGhostMarker(row.c.id);
        }
      }
      prevBottom = y + row.card.offsetHeight;
    }

    // Legacy cards without fallback go below the article, stable id-sorted.
    const articleBottom = articleTop + articleHeight;
    const withoutFb = state.activeComments
      .map(c => ({ c, card: state.cardEls.get(c.id) }))
      .filter(x => x.card && !state.anchorMarks.get(x.c.id) && !(x.c.anchor?.fallback && typeof x.c.anchor.fallback.ratio === 'number'))
      .sort((a, b) => (a.c.id || '').localeCompare(b.c.id || ''));
    let tailY = Math.max(articleBottom + 32, prevBottom + margin);
    for (const { card } of withoutFb) {
      card.style.top = tailY + 'px';
      card.style.left = cardLeft + 'px';
      card.classList.add('odoc-unanchored');
      tailY += card.offsetHeight + margin;
    }
  }

  function renderGhostMarker(commentId, pageY) {
    let g = document.querySelector(`.odoc-ghost-marker[data-comment-id="${CSS.escape(commentId)}"]`);
    if (!g) {
      g = document.createElement('div');
      g.className = 'odoc-ghost-marker';
      g.dataset.commentId = commentId;
      document.body.appendChild(g);
    }
    g.style.top = pageY + 'px';
  }
  function removeGhostMarker(commentId) {
    const g = document.querySelector(`.odoc-ghost-marker[data-comment-id="${CSS.escape(commentId)}"]`);
    if (g) g.remove();
  }

  function setActiveComment(id) {
    state.activeId = id || null;
    document.querySelectorAll('.odoc-anchor-mark.active, .odoc-margin-comment.active, .odoc-element-outline.active')
      .forEach(el => el.classList.remove('active'));
    if (!id) { rebuildSharedHighlights(); return; }
    const mark = state.anchorMarks.get(id);
    if (mark?.el?.classList) mark.el.classList.add('active');
    const card = state.cardEls.get(id);
    card?.classList.add('active');
    rebuildSharedHighlights();
    // Do NOT reposition cards on click — only the .active highlight should
    // change. Reordering cards every click is disorienting; users expect
    // stable positions and just the visual cue swap. Cards keep whatever
    // layout repositionCards() established at refresh/resize time.
    scrollAnchorIntoView(id);
  }

  function scrollAnchorIntoView(id) {
    const mark = state.anchorMarks.get(id);
    if (!mark) return;
    let anchorRect = null;
    // Prefer the underlying TARGET ELEMENT (canvas/img/video etc) over the
    // overlay outline div — same rect, but more semantically correct.
    if (mark.ranges?.[0]) anchorRect = mark.ranges[0].getBoundingClientRect();
    else if (mark.targetEl?.getBoundingClientRect) anchorRect = mark.targetEl.getBoundingClientRect();
    else if (mark.el?.getBoundingClientRect) anchorRect = mark.el.getBoundingClientRect();
    if (!anchorRect) return;

    // We consider the anchor "comfortably visible" if its top is between the
    // bar (44px) and 60% of the viewport. Otherwise smooth-scroll so it lands
    // in the upper third — readable, with room for the card next to it.
    const barH = 44;
    const top = anchorRect.top;
    const vpH = window.innerHeight;
    const comfortableMin = barH + 80;
    const comfortableMax = vpH * 0.6;
    if (top >= comfortableMin && top <= comfortableMax) return;
    const targetTop = vpH * 0.25;          // land at 25% of viewport
    const delta = top - targetTop;
    window.scrollBy({ top: delta, behavior: 'smooth' });
  }

  // ========== Element outlines (saved + pending) ==========
  function outlineElement(comment) {
    const el = findElement(comment.anchor);
    if (!el) return null;
    const outline = document.createElement('div');
    outline.className = 'odoc-element-outline';
    outline.dataset.commentId = comment.id;
    document.body.appendChild(outline);
    const repos = () => positionOutlineAround(outline, el);
    repos();
    outline._reposition = repos;
    outline._targetEl = el;
    outline.style.pointerEvents = 'none';
    return { el: outline, targetEl: el };
  }

  // Tear down every per-comment artifact before a refresh: highlights, fallback
  // spans, outlines (preserving the in-flight 'pending' one), margin cards, and
  // both lookup maps. Anchored state must be reconstructed from the fresh list.
  function resetAnchors() {
    clearAllCommentHighlights();
    unwrapFallbackSpans();
    document.querySelectorAll('.odoc-element-outline:not(.pending)').forEach(el => el.remove());
    document.querySelectorAll('.odoc-ghost-marker').forEach(el => el.remove());
    for (const card of commentLayer.querySelectorAll('.odoc-margin-comment')) card.remove();
    state.anchorMarks.clear();
    state.cardEls.clear();
  }

  // ========== refreshComments ==========
  async function refreshComments() {
    resetAnchors();

    let list = [];
    if (isFork) {
      // Read-only: parse the embedded JSON. No /api calls.
      const block = document.getElementById('odoc-fork-comments');
      if (block) {
        try { list = (JSON.parse(block.textContent || '{}').comments) || []; } catch { list = []; }
      }
    } else {
      try {
        const r = await fetch(`/v1/comments?slug=${encodeURIComponent(slug)}&version=${version}`);
        list = (await r.json()).data;
      } catch { list = []; }
    }
    state.activeComments = list.filter(c => c.status !== 'resolved');
    document.body.classList.toggle('odoc-has-comments', state.activeComments.length > 0);
    document.body.dataset.odocReady = '1';

    const fabCount = document.getElementById('odoc-fab-count');
    if (fabCount) fabCount.textContent = state.activeComments.length;

    const textCache = state.activeComments.some(c => (c.anchor?.kind || (c.anchor?.text ? 'text' : null)) === 'text')
      ? collectTextNodes() : null;
    for (const comment of state.activeComments) {
      const kind = comment.anchor?.kind || (comment.anchor?.text ? 'text' : null);
      if (kind === 'text') {
        const range = findTextRange(comment.anchor, textCache);
        if (range) {
          if (HIGHLIGHT_API) {
            state.anchorMarks.set(comment.id, { kind: 'text', ranges: [range] });
          } else {
            const span = fallbackWrapAsSpan(comment, range);
            if (span) {
              span.addEventListener('click', (e) => { e.stopPropagation(); setActiveComment(comment.id); });
              span.style.cursor = 'pointer';
              state.anchorMarks.set(comment.id, { kind: 'text', el: span });
            }
          }
        }
      } else if (kind === 'element') {
        const out = outlineElement(comment);
        if (out) {
          out.targetEl.addEventListener('click', (e) => { e.stopPropagation(); setActiveComment(comment.id); });
          if (out.targetEl.style) out.targetEl.style.cursor = 'pointer';
          state.anchorMarks.set(comment.id, { kind: 'element', el: out.el, targetEl: out.targetEl });
        }
      }
      const card = buildCard(comment);
      commentLayer.appendChild(card);
      state.cardEls.set(comment.id, card);
    }
    rebuildSharedHighlights();
    evaluateLayout();
    requestAnimationFrame(repositionCards);
  }

  // Click on a Highlight-API range → activate. Highlight API has no per-range
  // event so we delegate from a root click handler by hit-testing ranges.
  function findCommentAtPoint(x, y) {
    if (!HIGHLIGHT_API) return null;
    for (const [id, mark] of state.anchorMarks) {
      if (!mark.ranges) continue;
      for (const r of mark.ranges) {
        const rects = r.getClientRects();
        for (let i = 0; i < rects.length; i++) {
          const rect = rects[i];
          if (x >= rect.left && x <= rect.right && y >= rect.top && y <= rect.bottom) return id;
        }
      }
    }
    return null;
  }

  // ========== Narrow mode (single source of truth) ==========
  function evaluateLayout() {
    const MIN_ARTICLE_WIDTH = 400;
    const MIN_COLUMN_WIDTH = 300;
    const isPhone = window.innerWidth < 700;
    const metrics = getArticleMetrics();
    const articleWidth = metrics.el === document.body ? Infinity : metrics.width;
    const articleRight = metrics.el === document.body ? 0 : metrics.right;
    const columnRoom = window.innerWidth - articleRight;
    const narrow = isPhone || articleWidth < MIN_ARTICLE_WIDTH || columnRoom < MIN_COLUMN_WIDTH;
    state.narrow = narrow;
    document.body.classList.toggle('odoc-narrow', narrow);
    fab.style.display = (narrow && state.activeComments.length > 0) ? 'inline-flex' : 'none';
    if (!narrow) commentLayer.classList.remove('open');
  }

  window.addEventListener('resize', () => requestAnimationFrame(() => { evaluateLayout(); repositionCards(); }));
  // Esc cancels re-anchor mode globally.
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && state.reanchoringId) exitReanchor();
  });
  window.addEventListener('scroll', () => requestAnimationFrame(repositionCards), { passive: true });
  if (window.ResizeObserver) new ResizeObserver(() => repositionCards()).observe(document.body);

  // ========== Auth (login seam) ==========
  // Login seam. There is no built-in login provider yet (commenting is
  // anonymous), so this is only reachable when the server reports
  // authConfigured=true — the hook a future Octo unified login plugs into. It
  // should redirect to / open the provider, then on return the session cookie is
  // set and renderIdentity()/refreshComments() pick up the new identity.
  function startLogin() {
    if (!authConfigured) return;
    alert('Sign-in is not configured on this server.');
  }

  // ========== Host handshake (OCT-171 postMessage receiver) ==========
  // Cross-origin login bootstrap when embedded in octo-web. Sender contract
  // (OCT-170, octo-web packages/dmworkbase/src/Components/DocTab/DocTab.tsx):
  //   iframe.onLoad → postMessage({ type: 'octo:init', token }, docOrigin)
  // Receiver responsibility: hand `token` to POST /v1/auth/login to mint the
  // HttpOnly odoc_sid cookie, then same-origin fetch takes over — token never
  // touches URL, storage, or logs. Trust boundary: cfg.hostOrigins (derived
  // server-side from FRAME_ANCESTORS); anything else is silently dropped.
  const hostOrigins = Array.isArray(cfg.hostOrigins) ? cfg.hostOrigins : [];
  let lastLoginToken = null; // one login per token; host re-mount changes it.
  async function refreshAuthMe() {
    // Pull identity/isOwner/authConfigured off the fresh cookie and repaint.
    // Failure keeps the pre-handshake state (anonymous) — no error UI, this
    // path is best-effort.
    try {
      const r = await fetch('/v1/auth/me', { credentials: 'include' });
      if (!r.ok) return;
      const d = await r.json();
      identity = d?.data?.identity || null;
      isOwner = !!(d?.data?.isOwner);
      renderIdentity();
      refreshComments();
    } catch (_) { /* silent — anonymous fallback */ }
  }
  window.addEventListener('message', (event) => {
    if (!hostOrigins.includes(event.origin)) return; // origin allowlist gate
    const data = event.data;
    if (!data || data.type !== 'octo:init') return;
    const token = typeof data.token === 'string' ? data.token : '';
    if (!token || token === lastLoginToken) return;
    lastLoginToken = token;
    // credentials:'include' so the Set-Cookie response sticks; body carries the
    // token in JSON only (never URL / query / storage / log).
    fetch('/v1/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token }),
      credentials: 'include',
    }).then((r) => { if (r.ok) refreshAuthMe(); /* 4xx/5xx → silent anon */ })
      .catch(() => { /* network error → silent anon */ });
  });

  // ========== Publish / Share modals ==========
  function closeAuxModal() {
    const m = document.getElementById('odoc-aux-modal');
    if (m) m.remove();
  }
  function showPublishModal() {
    closeAuxModal();
    const bg = document.createElement('div');
    bg.className = 'odoc-modal-bg';
    bg.id = 'odoc-aux-modal';
    bg.innerHTML = `
      <div class="odoc-modal" data-state="idle">
        <h3>Publish this doc</h3>
        <p>We'll publish this to your self-hosted octo-doc server so anyone with the link can read and comment.</p>
        <div class="step"><span class="n">·</span><span>Slug: <code id="odoc-pub-slug">${escapeHtml(slug)}</code></span></div>
        <div class="status" id="odoc-pub-status" style="margin-top:10px;display:none;"></div>
        <div id="odoc-pub-result" style="margin-top:10px;display:none;">
          <div class="code" style="font-size:14px;letter-spacing:0;text-align:left;" id="odoc-pub-url"></div>
          <div class="actions" style="justify-content:flex-start;gap:8px;">
            <button class="primary" id="odoc-pub-copy">Copy link</button>
            <button id="odoc-pub-open">View live →</button>
          </div>
        </div>
        <div class="actions">
          <button id="odoc-pub-cancel">Cancel</button>
          <button class="primary" id="odoc-pub-go">Publish</button>
        </div>
      </div>`;
    document.body.appendChild(bg);
    document.getElementById('odoc-pub-cancel').onclick = closeAuxModal;
    document.getElementById('odoc-pub-go').onclick = async () => {
      const status = document.getElementById('odoc-pub-status');
      const go = document.getElementById('odoc-pub-go');
      status.style.display = 'block';
      status.textContent = 'Publishing — this can take 20–60s on first run…';
      go.disabled = true;
      try {
        const r = await fetch('/v1/publish', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ slug })
        });
        const data = await r.json();
        if (!r.ok || data.error) {
          status.textContent = 'Failed: ' + errMsg(data, r.status);
          go.disabled = false;
          return;
        }
        const url = data.data.url;
        status.style.display = 'none';
        const result = document.getElementById('odoc-pub-result');
        result.style.display = 'block';
        document.getElementById('odoc-pub-url').textContent = url;
        document.getElementById('odoc-pub-copy').onclick = () => navigator.clipboard?.writeText(url);
        document.getElementById('odoc-pub-open').onclick = () => window.open(url, '_blank');
        document.getElementById('odoc-pub-go').style.display = 'none';
        document.getElementById('odoc-pub-cancel').textContent = 'Done';
      } catch (e) {
        status.textContent = 'Failed: ' + e.message;
        go.disabled = false;
      }
    };
  }
  function showShareModal() {
    closeAuxModal();
    const bg = document.createElement('div');
    bg.className = 'odoc-modal-bg';
    bg.id = 'odoc-aux-modal';
    bg.innerHTML = `
      <div class="odoc-modal">
        <h3>Share this doc</h3>
        <p class="muted" style="margin:0 0 10px;">Generate a link that lets anyone read and comment — no account needed.</p>
        <div class="status" id="odoc-share-status" style="display:none;margin-bottom:10px;"></div>
        <div id="odoc-share-result" style="display:none;">
          <div class="code" id="odoc-share-url" style="font-size:14px;letter-spacing:0;text-align:left;cursor:copy;"></div>
          <div class="actions" style="justify-content:flex-start;gap:8px;margin-top:0;margin-bottom:10px;">
            <button class="primary" id="odoc-share-copy">Copy link</button>
          </div>
          <p class="muted" style="font-size:12px;">Anyone with this link can read and comment. Generate a new link to rotate it (old links stop working).</p>
        </div>
        <div class="actions">
          <button id="odoc-share-close">Close</button>
          <button class="primary" id="odoc-share-go">Generate link</button>
        </div>
      </div>`;
    document.body.appendChild(bg);
    document.getElementById('odoc-share-close').onclick = closeAuxModal;
    document.getElementById('odoc-share-go').onclick = async () => {
      const status = document.getElementById('odoc-share-status');
      const go = document.getElementById('odoc-share-go');
      status.style.display = 'block';
      status.textContent = 'Generating…';
      go.disabled = true;
      try {
        const r = await fetch(`/v1/docs/${encodeURIComponent(slug)}/share`, { method: 'POST' });
        const data = await r.json();
        if (!r.ok || data.error) {
          status.textContent = 'Failed: ' + errMsg(data, r.status);
          go.disabled = false;
          return;
        }
        const url = data.data.url;
        status.style.display = 'none';
        const result = document.getElementById('odoc-share-result');
        result.style.display = 'block';
        const urlEl = document.getElementById('odoc-share-url');
        urlEl.textContent = url;
        urlEl.onclick = () => navigator.clipboard?.writeText(url);
        document.getElementById('odoc-share-copy').onclick = () => navigator.clipboard?.writeText(url);
        go.textContent = 'Rotate link';
        go.disabled = false;
      } catch (e) {
        status.textContent = 'Failed: ' + e.message;
        go.disabled = false;
      }
    };
  }

  // Draft mode: promote the current draft to an immutable published version.
  function showPromoteModal() {
    closeAuxModal();
    const bg = document.createElement('div');
    bg.className = 'odoc-modal-bg';
    bg.id = 'odoc-aux-modal';
    bg.innerHTML = `
      <div class="odoc-modal">
        <h3>Publish this draft</h3>
        <p class="muted">This freezes the current draft as a new immutable version with a permanent URL. The draft slot is then cleared — start a fresh draft to iterate again.</p>
        <div class="status" id="odoc-pub-status" style="display:none;margin-top:10px;"></div>
        <div id="odoc-pub-result" style="display:none;margin-top:10px;">
          <div class="code" id="odoc-pub-url" style="font-size:14px;letter-spacing:0;text-align:left;"></div>
          <div class="actions" style="justify-content:flex-start;gap:8px;">
            <button class="primary" id="odoc-pub-copy">Copy link</button>
            <button id="odoc-pub-open">View →</button>
          </div>
        </div>
        <div class="actions">
          <button id="odoc-pub-cancel">Cancel</button>
          <button class="primary" id="odoc-pub-go">Publish</button>
        </div>
      </div>`;
    document.body.appendChild(bg);
    document.getElementById('odoc-pub-cancel').onclick = closeAuxModal;
    document.getElementById('odoc-pub-go').onclick = async () => {
      const status = document.getElementById('odoc-pub-status');
      const go = document.getElementById('odoc-pub-go');
      status.style.display = 'block';
      status.textContent = 'Publishing…';
      go.disabled = true;
      try {
        const r = await fetch(`/v1/docs/${encodeURIComponent(slug)}/draft/promote`, {
          method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}',
        });
        const data = await r.json();
        if (!r.ok || data.error) {
          status.textContent = 'Failed: ' + errMsg(data, r.status);
          go.disabled = false;
          return;
        }
        const url = data.data.url;
        status.style.display = 'none';
        const result = document.getElementById('odoc-pub-result');
        result.style.display = 'block';
        document.getElementById('odoc-pub-url').textContent = url;
        document.getElementById('odoc-pub-copy').onclick = () => navigator.clipboard?.writeText(url);
        document.getElementById('odoc-pub-open').onclick = () => window.open(url, '_blank');
        document.getElementById('odoc-pub-go').style.display = 'none';
        document.getElementById('odoc-pub-cancel').textContent = 'Done';
      } catch (e) {
        status.textContent = 'Failed: ' + e.message;
        go.disabled = false;
      }
    };
  }

  // ========== Popup (new-comment): text + element anchors ==========
  let popup = null;
  let pendingElementOutline = null;

  function setPendingTextHighlight(range) {
    if (!HIGHLIGHT_API || !range) return;
    pendingHighlight.clear();
    pendingHighlight.add(range);
  }
  function clearPendingTextHighlight() {
    if (HIGHLIGHT_API) pendingHighlight.clear();
  }
  function setPendingElementOutline(el) {
    clearPendingElementOutline();
    pendingElementOutline = document.createElement('div');
    pendingElementOutline.className = 'odoc-element-outline pending';
    positionOutlineAround(pendingElementOutline, el);
    document.body.appendChild(pendingElementOutline);
  }
  function clearPendingElementOutline() {
    if (pendingElementOutline) { pendingElementOutline.remove(); pendingElementOutline = null; }
  }
  function closePopup() {
    if (popup) { popup.remove(); popup = null; }
    clearPendingTextHighlight();
    clearPendingElementOutline();
  }

  function openPopup(anchor, rect) {
    if (isFork) return; // read-only fork view: no new comments
    closePopup();
    hideHoverUI();
    popup = document.createElement('div');
    popup.className = 'odoc-popup';
    const needsSignIn = authConfigured && !identity;
    const preview = anchor.kind === 'text'
      ? `"${escapeHtml(anchor.text.slice(0, 80))}${anchor.text.length > 80 ? '…' : ''}"`
      : `📎 ${escapeHtml(anchor.label)}`;
    popup.innerHTML = `
      <div class="head"><span class="h">${preview}</span><span class="x">×</span></div>
      ${needsSignIn ? '<div class="signin-needed">Sign in to comment.</div>' : ''}
      <textarea placeholder="What should change?" ${needsSignIn ? 'disabled' : ''}></textarea>
      <div class="foot">
        <span class="hint">${needsSignIn ? '' : '⌘+Enter to submit'}</span>
        <button class="submit">${needsSignIn ? 'Sign in' : 'Comment'}</button>
      </div>`;
    // Default: open below `rect` (used for text-selection popups so it follows
    // the cursor). For element anchors invoked via the Comment pill, we want
    // the popup to open ABOVE the pill so it doesn't dive into the artifact
    // body. The caller signals this by setting anchor._placeAbove = true.
    document.body.appendChild(popup);   // append first so offsetHeight is known
    const popupH = popup.offsetHeight || 140;
    if (anchor._placeAbove && rect.top - 8 - popupH >= 8) {
      popup.style.top = (window.scrollY + rect.top - popupH - 8) + 'px';
    } else {
      popup.style.top = (window.scrollY + rect.bottom + 8) + 'px';
    }
    const left = Math.min(rect.left + window.scrollX, window.innerWidth - 340);
    popup.style.left = Math.max(8, left) + 'px';

    if (anchor.kind === 'text' && anchor._range) {
      setPendingTextHighlight(anchor._range);
      window.getSelection()?.removeAllRanges();
    } else if (anchor.kind === 'element' && anchor._el) {
      setPendingElementOutline(anchor._el);
    }

    const textarea = popup.querySelector('textarea');
    // Defer focus past the click cycle that follows mouseup — otherwise the
    // root click handler can steal focus back and the user has to click the
    // popup before they can type.
    if (!needsSignIn) requestAnimationFrame(() => textarea.focus());
    popup.querySelector('.x').onclick = closePopup;

    const submit = async () => {
      if (needsSignIn) { closePopup(); startLogin(); return; }
      const text = textarea.value.trim();
      if (!text) return;
      // Capture a fallback position so the card can stay roughly in place
      // even when the anchor text is later rewritten. articleY is the
      // anchor's vertical center, measured as a fraction of the article's
      // height — stable across viewport widths. nearestHeading is the id
      // (or text) of the closest preceding h1/h2/h3, used as a structural
      // landmark if the text-anchor fails entirely.
      const fallback = captureFallbackPosition(anchor);
      const sendAnchor = anchor.kind === 'text'
        ? { kind: 'text', text: anchor.text, context_before: anchor.context_before, context_after: anchor.context_after, fallback }
        : { kind: 'element', selector: anchor.selector, label: anchor.label,
            // IDENTITY-FIRST: persist the worker-stamped artifact id so
            // future resolution is by content identity, not DOM position.
            // Same artifact in any future version = same aid.
            aid: anchor._el ? elementAid(anchor._el) : null,
            // Fingerprint is the legacy fallback for any pre-aid docs.
            fingerprint: anchor._el ? elementFingerprint(anchor._el) : null,
            fallback };
      const r = await fetch('/v1/comments', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ slug, version, anchor: sendAnchor, text })
      });
      if (r.status === 401) { closePopup(); startLogin(); return; }
      await r.json().catch(() => null);
      closePopup();
      await refreshComments();
    };
    popup.querySelector('.submit').onclick = submit;
    textarea.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submit();
      if (e.key === 'Escape') closePopup();
    });
  }

  // Capture position metadata at create time. Used when the saved text
  // anchor no longer resolves (the doc was rewritten) — the card still
  // lands near the original location instead of falling to the bottom.
  function captureFallbackPosition(anchor) {
    const metrics = getArticleMetrics();
    const articleEl = metrics.el || document.body;
    const articleTop = articleEl.getBoundingClientRect().top + window.scrollY;
    const articleHeight = Math.max(1, articleEl.scrollHeight);
    let rect = null;
    if (anchor.kind === 'text' && anchor._range) rect = anchor._range.getBoundingClientRect();
    else if (anchor.kind === 'element' && anchor._el) rect = anchor._el.getBoundingClientRect();
    if (!rect) return null;
    const centerY = rect.top + rect.height / 2 + window.scrollY;
    const ratio = Math.max(0, Math.min(1, (centerY - articleTop) / articleHeight));
    // Find the nearest preceding heading for a structural landmark.
    let nearestHeading = null;
    const headings = document.querySelectorAll('h1, h2, h3');
    for (const h of headings) {
      if (h.closest(UI_CONTAINERS)) continue;
      const hr = h.getBoundingClientRect();
      if (hr.top + window.scrollY <= centerY) {
        nearestHeading = { id: h.id || null, text: h.textContent.trim().slice(0, 80) };
      } else break;
    }
    return { ratio, nearestHeading };
  }

  function getContext(range, chars) {
    // Use the same flattened-text view that findTextRange searches, so saved
    // context can disambiguate hits across element boundaries.
    try {
      const { nodes, total } = collectTextNodes();
      const startLoc = nodes.find(n => n.node === range.startContainer);
      const endLoc = nodes.find(n => n.node === range.endContainer);
      if (!startLoc || !endLoc) return { before: '', after: '' };
      const startG = startLoc.start + range.startOffset;
      const endG = endLoc.start + range.endOffset;
      return {
        before: total.slice(Math.max(0, startG - chars), startG),
        after: total.slice(endG, endG + chars),
      };
    } catch { return { before: '', after: '' }; }
  }

  // ========== Drag-to-comment on artifacts ==========
  // Commentable artifacts: leaf media + semantic blocks the author signaled
  // are "a unit" (section/article/aside/blockquote/table/details — note
  // `figure` and `pre` already included as media) + any element the author
  // explicitly opted in via `data-odoc-artifact` or a class containing
  // `odoc-artifact`. Author-composed cards (a transcript panel built from
  // <div>s, a custom widget) become commentable as a unit when tagged —
  // instead of being invisible to the artifact system.
  // NB: `article` is excluded — it's a doc content-root pattern; making it
  // commentable would let the whole doc become one big artifact. Use
  // `section` or `data-odoc-artifact` to mark sub-blocks instead.
  const COMMENTABLE =
    'img, svg, canvas, video, pre, figure, iframe[src], ' +
    'section, aside, blockquote, table, details, ' +
    '[data-odoc-artifact], [class*="odoc-artifact"]';
  // The doc content root (per SKILL.md every doc wraps content in one of
  // these). resolveArtifact must never climb into/past it.
  const ARTICLE_ROOT_SEL = 'main, article, .wrap, .content, .container';
  const DRAG_THRESHOLD = 5;
  let dragState = null;

  function isInUI(el) {
    return el && el.closest && el.closest(UI_ALL);
  }

  // Resolve the *meaningful* artifact boundary for a hovered/hit leaf.
  //
  // COMMENTABLE only lists leaf media (img/svg/canvas/video/pre/figure/
  // iframe). Docs frequently compose ONE visual artifact out of <div>s
  // wrapping a nested media element — e.g. a phone mockup
  // <div class="phone"> … <svg> progress ring </svg> … </div>.
  // `closest(COMMENTABLE)` resolves to the inner <svg> (the ring), so the
  // outline/anchor hugs a tiny inner region instead of the whole mockup.
  //
  // The robust signal for "this is the artifact the author designed as one
  // unit" is NOT an id or an area ratio — it's a *visual container box*:
  // an ancestor the author gave its own visual boundary (background,
  // border, border-radius, box-shadow, or a fixed/aspect-ratio size).
  // The phone mockup has background+border-radius+box-shadow+aspect-ratio;
  // the inner `.screen`/`.ring-wrap` are pure layout flexers with none.
  //
  // Algorithm: climb from the media leaf to the OUTERMOST visual-box
  // ancestor that is still tighter than the content column. <figure> is a
  // definitive unit. Stop at the doc content root / UI / <body>. This is
  // resilient to viewport width (no innerWidth break that truncates the
  // climb before reaching the real artifact) and needs no id.
  let _csCache = null, _csCacheEl = null;
  function cs(el) {
    if (_csCacheEl === el && _csCache) return _csCache;
    try { _csCache = getComputedStyle(el); } catch (e) { _csCache = null; }
    _csCacheEl = el;
    return _csCache;
  }
  // Does this element have an author-given visual boundary (i.e. it reads
  // as a self-contained "card/frame/mockup", not a transparent layout div)?
  function isVisualBox(el) {
    if (!el || el.nodeType !== 1 || el === document.body) return false;
    const tag = el.tagName.toLowerCase();
    if (tag === 'figure' || tag === 'img' || tag === 'svg' || tag === 'canvas' || tag === 'video') return true;
    const s = cs(el);
    if (!s) return false;
    const hasBg =
      (s.backgroundImage && s.backgroundImage !== 'none') ||
      (s.backgroundColor &&
        s.backgroundColor !== 'rgba(0, 0, 0, 0)' &&
        s.backgroundColor !== 'transparent');
    const hasBorder =
      (s.borderTopWidth && parseFloat(s.borderTopWidth) > 0) ||
      (s.borderBottomWidth && parseFloat(s.borderBottomWidth) > 0) ||
      (s.borderLeftWidth && parseFloat(s.borderLeftWidth) > 0) ||
      (s.borderRightWidth && parseFloat(s.borderRightWidth) > 0);
    const hasRadius = s.borderRadius && s.borderRadius !== '0px' && parseFloat(s.borderRadius) > 0;
    const hasShadow = s.boxShadow && s.boxShadow !== 'none';
    const hasAspect = s.aspectRatio && s.aspectRatio !== 'auto';
    return !!(hasBg || hasBorder || hasRadius || hasShadow || hasAspect);
  }
  function isFullWidthBand(el) {
    const r = el.getBoundingClientRect();
    if (!r.width) return true;
    // Compare against the article column, not the viewport: a full-bleed
    // showcase wrapper spans the column; the artifact inside it does not.
    const root = articleRootEl();
    const colW = root ? root.getBoundingClientRect().width : window.innerWidth;
    return r.width >= Math.max(1, colW) * 0.92;
  }
  function articleRootEl() {
    try {
      const c = document.querySelector(ARTICLE_ROOT_SEL);
      if (c && !(c.closest && (c.closest(UI_ALL)))) return c;
    } catch (e) {}
    return null;
  }
  // True if `node` sits within (or is) a resolved artifact — including the
  // wrapper region around a nested media leaf. Used to keep text-marquee
  // drags from starting on composite artifacts (e.g. the phone mockup's
  // padding, which is a <div>, not a COMMENTABLE leaf).
  function isWithinArtifact(node) {
    if (!node || node.nodeType !== 1) return false;
    const direct = node.matches(COMMENTABLE) ? node : node.closest(COMMENTABLE);
    if (direct) return true;
    // Climb: is any ancestor a resolved-artifact wrapper that contains a
    // COMMENTABLE descendant? (cheap walk, capped)
    let el = node, guard = 0;
    while (el && el !== document.body && guard++ < 14) {
      if (
        el.querySelector &&
        el.querySelector(COMMENTABLE) &&
        resolveArtifact(el.querySelector(COMMENTABLE)) === el
      ) {
        return true;
      }
      el = el.parentElement;
    }
    return false;
  }
  function resolveArtifact(leaf) {
    if (!leaf || leaf.nodeType !== 1) return leaf;
    // If the leaf is already inside a comment-anchored element, keep that
    // exact element so existing anchors don't shift.
    if (leaf.closest && leaf.closest('[data-odoc-anchored]')) {
      return leaf.closest('[data-odoc-anchored]');
    }
    // Climb the full ancestor chain up to the content root, recording the
    // OUTERMOST visual-box ancestor that is still tighter than the content
    // column. Crucially we DO NOT break early on a non-visual layout div
    // (the inner `.screen`/`.ring-wrap` flexers): we climb THROUGH them so
    // a transparent wrapper between the media and the real mockup box can
    // never truncate the search before reaching the artifact.
    let el = leaf;
    let best = leaf;
    let guard = 0;
    while (el.parentElement && guard++ < 24) {
      const parent = el.parentElement;
      if (parent === document.body || parent.nodeType !== 1) break;
      if (parent.closest && (parent.closest(UI_ALL) || isInUI(parent))) break;
      // The doc's content root is a hard boundary — never the artifact.
      if (parent.matches && parent.matches(ARTICLE_ROOT_SEL)) break;
      if (parent.tagName && parent.tagName.toLowerCase() === 'figure') {
        return parent; // semantic artifact unit — definitive
      }
      // A visual box that still fits inside the column is a candidate
      // artifact boundary. Keep the OUTERMOST such box (so the whole phone
      // mockup wins over an inner card), but never a full-bleed band.
      if (isVisualBox(parent) && !isFullWidthBand(parent)) {
        best = parent;
      }
      el = parent;
    }
    return best;
  }

  // Given ANY node the cursor is over (the ring, a button, a label, the
  // empty padding — anything), return the artifact SECTION it belongs to,
  // or null if it isn't inside one. An artifact section is the OUTERMOST
  // ancestor (still inside the content column, never the content root) that
  // contains a media element (img/svg/canvas/video) — i.e. the whole
  // self-contained block the author composed. The entire section is one
  // unit: hovering anywhere inside it targets the same section, so the
  // Comment affordance never jumps as the cursor moves within it.
  // Resolves the COMMENTABLE artifact a hovered node belongs to.
  //
  // Old version was hard-coded around "must contain a media leaf
  // (img/svg/canvas/video)". That excluded the v0.1.54 cases — semantic
  // blocks (<section>, <table>, etc.) and author opt-in (data-odoc-artifact)
  // can be commentable WITHOUT containing any media. This rewrite mirrors
  // the COMMENTABLE selector exactly: an artifact is anything COMMENTABLE
  // (either as the hovered element itself, an ancestor of it, or a
  // commentable wrapper around a media leaf that IS the hovered element).
  function artifactSectionOf(node) {
    if (!node || node.nodeType !== 1) return null;
    if (isInUI(node) || (node.closest && node.closest(UI_ALL))) return null;
    // Existing anchored element wins (keep anchors stable).
    if (node.closest) {
      const anchored = node.closest('[data-odoc-anchored]');
      if (anchored) return anchored;
    }
    // 1. Direct hit: the hovered node IS a commentable artifact, OR it's
    //    inside one. closest() finds the NEAREST commentable ancestor.
    const direct = node.matches && node.matches(COMMENTABLE)
      ? node
      : (node.closest && node.closest(COMMENTABLE));
    if (direct && !isInUI(direct) && !(direct.matches && direct.matches(ARTICLE_ROOT_SEL))) {
      // Prefer the OUTERMOST commentable wrapper to handle the nesting case
      // (e.g. a card containing a media SVG — comment on the card, not the
      // svg, when the user hovers anywhere in the card). Climb past inner
      // commentables only when they're enclosed in another commentable
      // that's still inside the content column.
      let best = direct;
      let cur = direct.parentElement;
      let guard = 0;
      while (cur && cur !== document.body && guard++ < 20) {
        if (cur.matches && cur.matches(ARTICLE_ROOT_SEL)) break;
        if (cur.closest && (cur.closest(UI_ALL) || isInUI(cur))) break;
        if (cur.matches && cur.matches(COMMENTABLE) && !isFullWidthBand(cur)) {
          best = cur;
        }
        cur = cur.parentElement;
      }
      // resolveArtifact does final refinement (visual-box detection inside
      // the chosen section); honor it but only if it stays inside `best`.
      const refined = resolveArtifact(best);
      return (refined && best.contains && best.contains(refined)) ? refined : best;
    }
    // 2. Nothing commentable in this hover path. Don't show a pill.
    return null;
  }
  function rectsOverlap(a, b) { return !(a.right < b.left || a.left > b.right || a.bottom < b.top || a.top > b.bottom); }
  function findArtifactIntersecting(dragRect) {
    const sx = window.scrollX, sy = window.scrollY;
    for (const el of document.querySelectorAll(COMMENTABLE)) {
      if (isInUI(el)) continue;
      const resolved = resolveArtifact(el);
      const r = resolved.getBoundingClientRect();
      const pageRect = { left: r.left + sx, top: r.top + sy, right: r.right + sx, bottom: r.bottom + sy };
      if (rectsOverlap(pageRect, dragRect)) return resolved;
    }
    return null;
  }
  function elementSelector(el) {
    // IDENTITY FIRST: prefer the worker-stamped artifact id (immune to
    // DOM restructuring — same artifact in a different version has the
    // same aid).
    const aid = el.getAttribute && el.getAttribute('data-odoc-aid');
    if (aid) return `[data-odoc-aid="${aid}"]`;
    if (el.id) return `#${CSS.escape(el.id)}`;
    // Last-resort positional path (used only for previews before the doc
    // is published — after publish, every artifact has an aid).
    const parts = [];
    let cur = el;
    while (cur && cur.nodeType === 1 && cur !== document.body) {
      let part = cur.tagName.toLowerCase();
      if (cur.id) { parts.unshift(`#${CSS.escape(cur.id)}`); break; }
      const parent = cur.parentElement;
      if (parent) {
        const sameTag = Array.from(parent.children).filter(c => c.tagName === cur.tagName);
        if (sameTag.length > 1) part += `:nth-of-type(${sameTag.indexOf(cur) + 1})`;
      }
      parts.unshift(part);
      cur = parent;
    }
    return parts.join(' > ');
  }
  function elementAid(el) {
    return (el && el.getAttribute && el.getAttribute('data-odoc-aid')) || null;
  }
  function elementLabel(el) {
    return el.getAttribute('alt') || el.getAttribute('aria-label') || el.getAttribute('title') || el.tagName.toLowerCase();
  }

  // ── Anchor stability for ELEMENT (artifact) comments ──────────────────
  // Positional selectors like `div > svg:nth-of-type(1)` silently drift to
  // a DIFFERENT artifact when /odoc edit restructures the DOM (e.g. wraps
  // an svg in a <figure>, or adds a sibling). To make element anchors
  // survive regeneration we capture a CONTENT FINGERPRINT at comment time
  // and validate it at resolve time — if the selector lands on something
  // that isn't the same artifact, we treat the comment as unanchored
  // instead of pointing it at the wrong thing.
  function elementFingerprint(el) {
    if (!el || el.nodeType !== 1) return null;
    // Normalized, length-capped text content (collapses whitespace).
    const txt = (el.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 400);
    // Structural signature: ordered child tag names (one level) + svg/img
    // intrinsics, so two same-tag artifacts with different innards differ.
    const kids = Array.from(el.children).map(c => c.tagName.toLowerCase()).join(',');
    const dims = [
      el.getAttribute('viewBox') || '',
      el.getAttribute('src') || '',
      el.getAttribute('alt') || el.getAttribute('aria-label') || '',
    ].join('|');
    return {
      tag: el.tagName.toLowerCase(),
      text: txt,
      kids,
      meta: dims,
      // cheap stable hash so we can compare without storing huge strings
      h: cyrb53(el.tagName + '' + txt + '' + kids + '' + dims),
    };
  }
  // Small, fast 53-bit string hash (public-domain cyrb53).
  function cyrb53(str, seed = 0) {
    let h1 = 0xdeadbeef ^ seed, h2 = 0x41c6ce57 ^ seed;
    for (let i = 0, ch; i < str.length; i++) {
      ch = str.charCodeAt(i);
      h1 = Math.imul(h1 ^ ch, 2654435761);
      h2 = Math.imul(h2 ^ ch, 1597334677);
    }
    h1 = Math.imul(h1 ^ (h1 >>> 16), 2246822507) ^ Math.imul(h2 ^ (h2 >>> 13), 3266489909);
    h2 = Math.imul(h2 ^ (h2 >>> 16), 2246822507) ^ Math.imul(h1 ^ (h1 >>> 13), 3266489909);
    return (4294967296 * (2097151 & h2) + (h1 >>> 0)).toString(36);
  }
  // How well do two fingerprints match? 1 = identical artifact, 0 = no
  // relation. Tag mismatch is disqualifying. Otherwise weight exact-hash,
  // then text similarity, then structural (kids) similarity.
  function fingerprintScore(a, b) {
    if (!a || !b || a.tag !== b.tag) return 0;
    if (a.h === b.h) return 1;
    let s = 0;
    if (a.meta && a.meta === b.meta) s += 0.45;       // same viewBox/src/label
    if (a.kids && a.kids === b.kids) s += 0.25;        // same child structure
    if (a.text && b.text) {
      // token Jaccard on the normalized text
      const A = new Set(a.text.split(' ')), B = new Set(b.text.split(' '));
      let inter = 0; A.forEach(t => { if (B.has(t)) inter++; });
      const uni = A.size + B.size - inter;
      if (uni) s += 0.30 * (inter / uni);
    }
    return s;
  }

  document.addEventListener('mousedown', (e) => {
    if (e.button !== 0) return;
    const t = e.target;
    if (!t || t.nodeType !== 1 || isInUI(t)) return;
    if (t.closest('button, a, input, select, textarea, [contenteditable], [role="button"]')) return;
    if (isWithinArtifact(t)) return;
    dragState = { x0: e.pageX, y0: e.pageY, marquee: null, dragged: false };
  }, true);

  document.addEventListener('mousemove', (e) => {
    if (!dragState) return;
    const dx = e.pageX - dragState.x0, dy = e.pageY - dragState.y0;
    if (Math.hypot(dx, dy) < DRAG_THRESHOLD) return;
    dragState.dragged = true;
    const dragRect = {
      left: Math.min(dragState.x0, e.pageX), top: Math.min(dragState.y0, e.pageY),
      right: Math.max(dragState.x0, e.pageX), bottom: Math.max(dragState.y0, e.pageY),
    };
    const hit = findArtifactIntersecting(dragRect);
    if (hit) {
      if (!dragState.marquee) {
        dragState.marquee = document.createElement('div');
        dragState.marquee.className = 'odoc-drag-marquee';
        document.body.appendChild(dragState.marquee);
      }
      dragState.marquee.style.left = Math.min(dragState.x0, e.pageX) + 'px';
      dragState.marquee.style.top = Math.min(dragState.y0, e.pageY) + 'px';
      dragState.marquee.style.width = Math.abs(dx) + 'px';
      dragState.marquee.style.height = Math.abs(dy) + 'px';
    } else if (dragState.marquee) {
      dragState.marquee.remove(); dragState.marquee = null;
    }
  }, true);

  document.addEventListener('mouseup', (e) => {
    // Unified mouseup: drag-to-comment branch first, otherwise fall through to
    // text-selection-popup behavior. Single capture-phase listener avoids the
    // race where drag-end outside an artifact would still trigger the bubble-
    // phase selection-popup handler.
    const ds = dragState;
    if (ds) {
      const { x0, y0, dragged, marquee } = ds;
      dragState = null;
      if (marquee) marquee.remove();
      if (dragged) {
        const dragRect = {
          left: Math.min(x0, e.pageX), top: Math.min(y0, e.pageY),
          right: Math.max(x0, e.pageX), bottom: Math.max(y0, e.pageY),
        };
        const el = findArtifactIntersecting(dragRect);
        if (el) {
          e.preventDefault(); e.stopPropagation();
          hideHoverUI();
          openPopup({ kind: 'element', selector: elementSelector(el), label: elementLabel(el), _el: el }, el.getBoundingClientRect());
          return;
        }
        // Dragged but no artifact hit — likely a text selection. Fall through.
      }
    }
    maybeOpenSelectionPopup(e.target);
  }, true);

  // Mouse and touch both surface here. On iOS Safari long-press text-selection
  // does NOT fire mouseup, so we also listen for touchend. selectionchange
  // would seem cleaner but fires continuously during a drag — touchend gives
  // us a single "selection finished" signal.
  document.addEventListener('touchend', (e) => {
    const t = e.target || (e.changedTouches?.[0] && document.elementFromPoint(e.changedTouches[0].clientX, e.changedTouches[0].clientY));
    // Touchend fires before the OS finalizes selection — defer one tick.
    setTimeout(() => maybeOpenSelectionPopup(t), 0);
  }, true);

  function maybeOpenSelectionPopup(target) {
    // Selected text wins over "comment whole artifact." If there's a real text
    // selection, open the text-selection popup regardless of whether the
    // selection lives inside a commentable artifact. The hover pill remains
    // the path for "comment on the whole artifact" — they don't compete
    // because they're driven by different gestures (hover vs. drag-select).
    if (target && target.nodeType === 1 && isInUI(target)) return;
    const sel = window.getSelection();
    const text = sel && sel.toString().trim();
    if (!text || text.length < 2 || !sel.rangeCount) return;
    const anchorNode = sel.anchorNode;
    const anchorEl = anchorNode?.nodeType === 1 ? anchorNode : anchorNode?.parentElement;
    if (anchorEl && isInUI(anchorEl)) return;
    const range = sel.getRangeAt(0).cloneRange();
    const ctx = getContext(range, 60);
    // Re-anchor mode: rebind an existing unanchored comment to this selection
    // instead of creating a new one. Captured fallback position is refreshed
    // too so the comment "moves" to where the user just selected.
    if (state.reanchoringId) {
      const id = state.reanchoringId;
      exitReanchor();
      const newAnchor = {
        kind: 'text', text, context_before: ctx.before, context_after: ctx.after,
        fallback: captureFallbackPosition({ kind: 'text', _range: range }),
      };
      // Optimistic UI: drop the old anchor's highlight immediately so the
      // user never sees stale yellow on the previous location while the
      // PATCH is in flight. refreshComments() will repaint with the new
      // anchor once the server confirms.
      state.anchorMarks.delete(id);
      rebuildSharedHighlights();
      window.getSelection()?.removeAllRanges();
      fetch('/v1/comments', {
        method: 'PATCH', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ slug, id, anchor: newAnchor, version }),
      }).then(r => {
        if (r.status === 401) startLogin();
        return r.ok ? refreshComments() : null;
      });
      return;
    }
    const rect = range.getBoundingClientRect();
    openPopup({ kind: 'text', text, context_before: ctx.before, context_after: ctx.after, _range: range }, rect);
  }

  // Begin the re-anchor flow: future text selection on the doc will rebind
  // this comment instead of creating a new one. Toggle off if clicked again.
  function startReanchor(id) {
    if (state.reanchoringId === id) { exitReanchor(); return; }
    state.reanchoringId = id;
    document.body.classList.add('odoc-reanchoring');
  }
  function exitReanchor() {
    state.reanchoringId = null;
    document.body.classList.remove('odoc-reanchoring');
  }
  // Capture a fallback position for an existing comment by reading the
  // current anchor's location, so an unanchored card stays where it was.
  function fallbackFromExistingAnchor(commentId) {
    const mark = state.anchorMarks.get(commentId);
    if (!mark) return null;
    const metrics = getArticleMetrics();
    const articleEl = metrics.el || document.body;
    const articleTop = articleEl.getBoundingClientRect().top + window.scrollY;
    const articleHeight = Math.max(1, articleEl.scrollHeight);
    let rect = null;
    if (mark.ranges?.[0]) rect = mark.ranges[0].getBoundingClientRect();
    else if (mark.el) rect = mark.el.getBoundingClientRect();
    else if (mark.targetEl) rect = mark.targetEl.getBoundingClientRect();
    if (!rect) return null;
    const centerY = rect.top + rect.height / 2 + window.scrollY;
    return { ratio: Math.max(0, Math.min(1, (centerY - articleTop) / articleHeight)), nearestHeading: null };
  }
  // Wire banner buttons (created once near the bar). The banner is the
  // only place we expose "remove anchor" — keeps cards uncluttered and
  // resolves the gesture conflict you'd hit with "click empty space".
  document.getElementById('odoc-reanchor-cancel').onclick = (e) => { e.stopPropagation(); exitReanchor(); };
  document.getElementById('odoc-reanchor-remove').onclick = async (e) => {
    e.stopPropagation();
    const id = state.reanchoringId;
    if (!id) return;
    const fallback = fallbackFromExistingAnchor(id);
    exitReanchor();
    // Optimistic: clear the old highlight before the network call. If the
    // PATCH fails we'll just re-fetch and the anchor will return — no
    // worse than the pre-click state.
    state.anchorMarks.delete(id);
    rebuildSharedHighlights();
    const r = await fetch('/v1/comments', {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ slug, id, anchor: { kind: 'none', fallback }, version }),
    });
    if (r.status === 401) { startLogin(); return; }
    if (!r.ok) { const err = await r.json().catch(() => ({})); alert('Could not remove anchor: ' + errMsg(err, r.status)); return; }
    await refreshComments();
  };

  // ========== Hover affordance ==========
  // ========== Artifact hover affordance ==========
  // Hovering an unanchored commentable element (img/canvas/svg/video/pre)
  // shows: (1) a dashed blue outline around it, (2) a clickable "Comment" pill
  // in its top-right corner. Click the pill → opens the comment popup anchored
  // to that element. This is the discoverable path; drag-from-outside also
  // works for users who prefer that gesture.
  // The artifact section is ONE unit. Hovering anywhere inside it shows a
  // single Comment button anchored to the section's top-right corner — no
  // full outline. While the cursor stays anywhere within the same section
  // the button does not move or flicker.
  let commentPill = null, pillTargetEl = null;
  function showHoverUI(el) {
    if (isFork) return; // read-only: no new-comment affordances
    if (pillTargetEl === el && commentPill) return; // same section — keep as-is
    hideHoverUI();
    const r = el.getBoundingClientRect();

    commentPill = document.createElement('button');
    commentPill.className = 'odoc-comment-pill';
    commentPill.type = 'button';
    commentPill.setAttribute('aria-label', 'Comment on this');
    commentPill.innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>Comment`;
    // Float on the element's top-right EDGE — vertically centered on the top
    // border so the pill reads as attached to the whole element without covering
    // its content. Compact (~96px); clamp so it never runs off a narrow element's
    // left. Nudged 4px right so it overhangs the corner cleanly.
    const pillW = 96;
    commentPill.style.top = (window.scrollY + r.top - 13) + 'px';
    commentPill.style.left = (window.scrollX + Math.max(r.left + 8, r.right - pillW + 4)) + 'px';
    commentPill.onclick = (e) => {
      e.stopPropagation();
      e.preventDefault();
      const target = pillTargetEl;
      const pillRect = commentPill.getBoundingClientRect();
      hideHoverUI();
      if (!target) return;
      openPopup({
        kind: 'element',
        selector: elementSelector(target),
        label: elementLabel(target),
        _el: target,
        _placeAbove: true,
      }, pillRect);
    };
    pillTargetEl = el;
    document.body.appendChild(commentPill);
  }
  function hideHoverUI() {
    if (commentPill) { commentPill.remove(); commentPill = null; }
    pillTargetEl = null;
  }

  document.addEventListener('mouseover', (e) => {
    const t = e.target;
    if (!t || t.nodeType !== 1) return;
    // The pill itself is in `body` — don't hide UI when the cursor enters it.
    if (t.closest('.odoc-comment-pill')) return;
    if (isInUI(t)) { hideHoverUI(); return; }
    // ANY element under the cursor → the artifact section it belongs to
    // (the ring, a button, a label, empty padding — all map to the SAME
    // section). Hovering anywhere inside one artifact targets the whole
    // artifact as one unit.
    const section = artifactSectionOf(t);
    if (!section || isInUI(section)) { hideHoverUI(); return; }
    showHoverUI(section);
  });
  document.addEventListener('mouseout', (e) => {
    const next = e.relatedTarget;
    if (!next) { hideHoverUI(); return; }
    // Stay shown if cursor moves into the Comment button.
    if (next.closest && next.closest('.odoc-comment-pill')) return;
    // Stay shown while the cursor remains anywhere inside the SAME section.
    if (pillTargetEl && pillTargetEl.contains && pillTargetEl.contains(next)) return;
    if (pillTargetEl && artifactSectionOf(next) === pillTargetEl) return;
    if (isInUI(next)) hideHoverUI();
  });

  // ========== Selection → popup ==========
  // (See unified mouseup handler above — selection-popup branch lives in the
  // capture-phase handler so drag and selection cannot race.)

  // ========== Root click handler (delegated): menus, drawer, deselect, anchor click ==========
  document.addEventListener('click', (e) => {
    const t = e.target;
    if (!t || t.nodeType !== 1) return;

    // Close menus that aren't under the cursor
    if (!t.closest('#odoc-more-btn') && !t.closest('#odoc-secondary-menu')) secMenu.classList.remove('open');
    if (!t.closest('.odoc-menu-wrap')) copyMenu.classList.remove('open');
    // Close the profile menu on any click outside its wrapper.
    if (!t.closest('#odoc-me') && !t.closest('#odoc-me-menu')) {
      const mm = document.getElementById('odoc-me-menu');
      const mb = document.getElementById('odoc-me');
      if (mm) mm.classList.remove('open');
      if (mb) mb.setAttribute('aria-expanded', 'false');
    }
    if (!t.closest('.odoc-version-wrap')) {
      const vm = document.getElementById('odoc-version-menu');
      const vt = document.getElementById('odoc-version-toggle');
      if (vm) vm.classList.remove('open');
      if (vt) vt.setAttribute('aria-expanded', 'false');
    }
    if (!t.closest('.odoc-emoji-picker') && !t.closest('.odoc-react-add')) closeEmojiPicker();

    // Close drawer on outside click (narrow only)
    if (commentLayer.classList.contains('open') &&
        !t.closest('#odoc-comment-layer, .odoc-fab, .odoc-popup, .odoc-modal-bg, .odoc-emoji-picker')) {
      commentLayer.classList.remove('open');
    }

    // Custom-Highlight API: hit-test anchor ranges to detect anchor click.
    if (HIGHLIGHT_API && !isInUI(t)) {
      const hitId = findCommentAtPoint(e.clientX, e.clientY);
      if (hitId) { setActiveComment(hitId); return; }
    }

    // Deselect when clicking truly-outside the UI + outside any anchor/artifact.
    if (isInUI(t)) return;
    for (const mark of state.anchorMarks.values()) {
      const target = mark.targetEl || mark.el;
      if (target && (target === t || (target.contains && target.contains(t)))) return;
    }
    setActiveComment(null);
    const sel = window.getSelection();
    if (sel && sel.toString().trim() === '' && sel.rangeCount > 0) sel.removeAllRanges();
  });

  // ========== Copy as Markdown ==========
  function htmlToMarkdown(root) {
    function walk(node, ctx) {
      if (node.nodeType === Node.TEXT_NODE) {
        const t = node.nodeValue;
        if (ctx.inPre) return t;
        return t.replace(/\s+/g, ' ');
      }
      if (node.nodeType !== Node.ELEMENT_NODE) return '';
      if (node.classList && (
        node.classList.contains('odoc-bar') ||
        node.classList.contains('odoc-popup') ||
        node.classList.contains('odoc-margin-comment') ||
        node.classList.contains('odoc-modal-bg') ||
        node.classList.contains('odoc-element-outline') ||
        node.classList.contains('odoc-hover-outline') ||
        node.id === 'odoc-comment-layer'
      )) return '';
      const tag = node.tagName.toLowerCase();
      const kids = () => Array.from(node.childNodes).map(c => walk(c, ctx)).join('');
      switch (tag) {
        case 'h1': return '\n\n# ' + kids().trim() + '\n\n';
        case 'h2': return '\n\n## ' + kids().trim() + '\n\n';
        case 'h3': return '\n\n### ' + kids().trim() + '\n\n';
        case 'h4': return '\n\n#### ' + kids().trim() + '\n\n';
        case 'h5': return '\n\n##### ' + kids().trim() + '\n\n';
        case 'h6': return '\n\n###### ' + kids().trim() + '\n\n';
        case 'p': return '\n\n' + kids().trim() + '\n\n';
        case 'br': return '  \n';
        case 'hr': return '\n\n---\n\n';
        case 'strong': case 'b': return '**' + kids() + '**';
        case 'em': case 'i': return '*' + kids() + '*';
        case 'code': return ctx.inPre ? kids() : '`' + kids() + '`';
        case 'pre': {
          const c = { ...ctx, inPre: true };
          const lang = node.querySelector('code')?.className?.match(/language-([\w-]+)/)?.[1] || '';
          const inner = Array.from(node.childNodes).map(n => walk(n, c)).join('');
          return '\n\n```' + lang + '\n' + inner.replace(/\n$/, '') + '\n```\n\n';
        }
        case 'blockquote':
          return '\n\n' + kids().trim().split('\n').map(l => '> ' + l).join('\n') + '\n\n';
        case 'ul': {
          const items = Array.from(node.children).filter(c => c.tagName === 'LI');
          return '\n\n' + items.map(li => '- ' + walk(li, ctx).trim()).join('\n') + '\n\n';
        }
        case 'ol': {
          const items = Array.from(node.children).filter(c => c.tagName === 'LI');
          return '\n\n' + items.map((li, i) => (i + 1) + '. ' + walk(li, ctx).trim()).join('\n') + '\n\n';
        }
        case 'li': return kids();
        case 'a': {
          const href = node.getAttribute('href') || '';
          const text = kids().trim();
          return href ? `[${text}](${href})` : text;
        }
        case 'img': {
          const src = node.getAttribute('src') || '';
          const alt = node.getAttribute('alt') || '';
          return `![${alt}](${src})`;
        }
        case 'svg': case 'canvas': case 'video': case 'iframe':
          return `\n\n[${tag} embed]\n\n`;
        case 'figure': return '\n\n' + kids().trim() + '\n\n';
        case 'figcaption': return '\n\n*' + kids().trim() + '*\n\n';
        case 'table': {
          const rows = Array.from(node.querySelectorAll('tr'));
          if (!rows.length) return '';
          const cells = (r) => Array.from(r.children).map(c => walk(c, ctx).trim().replace(/\|/g, '\\|'));
          const head = cells(rows[0]);
          const body = rows.slice(1).map(cells);
          return '\n\n| ' + head.join(' | ') + ' |\n| ' + head.map(() => '---').join(' | ') + ' |\n' +
                 body.map(r => '| ' + r.join(' | ') + ' |').join('\n') + '\n\n';
        }
        case 'th': case 'td': case 'tr': return kids();
        default: return kids();
      }
    }
    return walk(root, { inPre: false }).replace(/\n{3,}/g, '\n\n').trim();
  }

  async function copyText(s) {
    try { await navigator.clipboard.writeText(s); return true; }
    catch {
      const ta = document.createElement('textarea');
      ta.value = s; ta.style.position = 'fixed'; ta.style.opacity = '0';
      document.body.appendChild(ta); ta.select();
      const ok = document.execCommand('copy');
      ta.remove();
      return ok;
    }
  }
  function flashCopied(btn) {
    if (!btn || btn.dataset.flashing === '1') return;
    btn.dataset.flashing = '1';
    const orig = btn.innerHTML;
    const oc = btn.style.color, ob = btn.style.borderColor;
    btn.innerHTML = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg><span>Copied</span>`;
    btn.style.color = '#3ecf8e'; btn.style.borderColor = '#3ecf8e';
    setTimeout(() => {
      btn.innerHTML = orig; btn.style.color = oc; btn.style.borderColor = ob;
      btn.dataset.flashing = '0';
    }, 1200);
  }
  function flashToast(msg) {
    const t = document.createElement('div');
    t.textContent = msg;
    t.style.cssText = 'position:fixed;bottom:18px;right:18px;background:#0a0a0a;color:#fff;padding:8px 14px;border-radius:6px;font:12px system-ui;z-index:1000001;opacity:0;transition:opacity 0.15s;pointer-events:none;box-shadow:0 4px 16px rgba(0,0,0,0.2);';
    document.body.appendChild(t);
    requestAnimationFrame(() => { t.style.opacity = '0.95'; });
    setTimeout(() => { t.style.opacity = '0'; setTimeout(() => t.remove(), 200); }, 1400);
  }
  function reactionsToMd(reactions) {
    if (!reactions) return '';
    const parts = Object.entries(reactions).filter(([, u]) => u && u.length > 0).map(([e, u]) => `${e} ${u.length}`);
    return parts.length ? `_reactions: ${parts.join(' · ')}_\n` : '';
  }
  function commentToMd(c) {
    const who = c.author ? `**@${c.author.login}**` : '*anonymous*';
    const when = new Date(c.created_at).toLocaleString();
    let anchorLine = '';
    if (c.anchor) {
      if (c.anchor.kind === 'element' || c.anchor.selector) anchorLine = `> _on ${c.anchor.label || c.anchor.selector}_\n`;
      else if (c.anchor.text) anchorLine = `> "${c.anchor.text.replace(/\n/g, ' ').slice(0, 200)}"\n`;
    }
    let md = `${who} — _${when}_\n${anchorLine}\n${c.text}\n${reactionsToMd(c.reactions)}`;
    if (Array.isArray(c.replies) && c.replies.length) {
      for (const r of c.replies) {
        const rwho = r.author ? `**@${r.author.login}**` : '*anonymous*';
        const rwhen = new Date(r.created_at).toLocaleString();
        md += `  ↳ ${rwho} — _${rwhen}_\n    ${r.text}\n    ${reactionsToMd(r.reactions)}`;
      }
    }
    return md;
  }

  window.__odocCopyDocMd = async function (includeComments) {
    const clone = document.body.cloneNode(true);
    clone.querySelectorAll(UI_ALL + ', script, style, noscript').forEach(n => n.remove());
    let md = htmlToMarkdown(clone);
    if (includeComments && state.activeComments.length) {
      md += '\n\n---\n\n## Comments\n\n' + state.activeComments.map(commentToMd).join('\n---\n\n');
    }
    const ok = await copyText(md);
    if (ok) flashCopied(document.getElementById('odoc-copy-md-btn'));
    else flashToast('Copy failed');
  };
  window.__odocCopyCommentMd = async function (commentId, srcBtn) {
    const c = state.activeComments.find(x => x.id === commentId);
    if (!c) return;
    const ok = await copyText(commentToMd(c));
    if (ok && srcBtn) {
      const origHTML = srcBtn.innerHTML, origColor = srcBtn.style.color;
      srcBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>';
      srcBtn.style.color = '#3ecf8e';
      setTimeout(() => { srcBtn.innerHTML = origHTML; srcBtn.style.color = origColor; }, 1200);
    } else if (!ok) flashToast('Copy failed');
  };

  // ========== Wire it up ==========
  refreshComments();
})();
