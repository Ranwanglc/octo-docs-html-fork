---
version: alpha
name: octo-doc
description: >-
  Visual identity for octo-doc — the overlay chrome injected into rendered docs
  (toolbar, comment pills, anchor highlights, FAB) and the landing/catalog pages.
  Tokens are extracted from assets/overlay.js and internal/transport/httpx.
colors:
  primary: "#1652f0"
  primaryHover: "#1245d0"
  ink: "#1a1a1a"
  inkStrong: "#111111"
  inkSoft: "#444444"
  muted: "#888888"
  mutedDark: "#666666"
  faint: "#aaaaaa"
  barText: "#555555"
  surface: "#ffffff"
  surfaceSubtle: "#f5f6f8"
  surfaceHover: "#e5e6ea"
  border: "#e5e5e7"
  hairline: "#eeeeee"
  inkPanel: "#0a0a0a"
  codeBg: "#f0f0ee"
  preBg: "#f7f7f5"
  preBorder: "#e8e7e3"
  quoteRule: "#d9d8d3"
  quoteText: "#6b6a66"
  highlight: "#fff7d0"
  highlightActive: "#ffd84d"
  danger: "#cc3333"
  accent: "#3ecf8e"
  okBg: "#e8f5ed"
  okFg: "#1a7340"
  warnBg: "#fff4dc"
  warnFg: "#8a5a00"
  askBg: "#ffe7e7"
  askFg: "#a52323"
  mineBg: "#e8eeff"
  agentBg: "#f3eaff"
  agentBorder: "#c3a8f0"
  agentFg: "#5a2da8"
typography:
  h1:
    fontFamily: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif
    fontSize: "38px"
    fontWeight: 700
  h1-page:
    fontFamily: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif
    fontSize: "30px"
    fontWeight: 700
  body-md:
    fontFamily: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif
    fontSize: "17px"
    fontWeight: 400
  ui:
    fontFamily: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif
    fontSize: "13px"
    fontWeight: 400
  ui-strong:
    fontFamily: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif
    fontSize: "13px"
    fontWeight: 600
  label-sm:
    fontFamily: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif
    fontSize: "12px"
    fontWeight: 400
  mono:
    fontFamily: ui-monospace, "SF Mono", Menlo, monospace
    fontSize: "13px"
    fontWeight: 400
rounded:
  sm: "4px"
  md: "6px"
  lg: "10px"
  xl: "12px"
  pill: "999px"
spacing:
  xs: "6px"
  sm: "8px"
  md: "12px"
  lg: "16px"
  xl: "20px"
components:
  bar:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    typography: "{typography.ui}"
    height: "48px"
    padding: "0 12px"
  button-primary:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.surface}"
    typography: "{typography.ui-strong}"
    rounded: "{rounded.md}"
    padding: "7px 14px"
  button-primary-hover:
    backgroundColor: "{colors.primaryHover}"
    textColor: "{colors.surface}"
  fab:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.surface}"
    typography: "{typography.ui-strong}"
    rounded: "{rounded.pill}"
    padding: "10px 16px"
  anchor-mark:
    backgroundColor: "{colors.highlight}"
    textColor: "{colors.ink}"
  anchor-mark-active:
    backgroundColor: "{colors.highlightActive}"
    textColor: "{colors.inkStrong}"
  comment-pill:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.surface}"
    typography: "{typography.label-sm}"
    rounded: "{rounded.pill}"
    padding: "6px 11px"
  comment-composer:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    typography: "{typography.ui}"
    rounded: "{rounded.lg}"
    padding: "14px"
---

# octo-doc

## Overview

octo-doc's visual identity is **unobtrusive editorial chrome over untrusted
content**. The product renders an arbitrary user-authored HTML document and
injects a thin overlay — a top toolbar, a floating comment button, inline anchor
highlights, and comment pills — so the design language must never compete with
the document it wraps. The aesthetic is **quiet, system-native, high-contrast**:
neutral surfaces, near-black ink, a single confident blue for every action, and
a warm amber used *only* to mark commented text. Everything uses the platform
UI font at small sizes so the overlay reads as part of the browser, not the page.

The two audiences are the same in style but different in scale: **rendered docs**
(`/d/<slug>/v/<n>`) get the overlay chrome; **landing and catalog pages** reuse
the same palette and type ramp at page scale.

## Colors

- **primary** `#1652f0` — the sole action color. Every button, link, the FAB, the
  current-version marker, and focus affordance uses it. One blue, used
  consistently, is the whole interaction story.
- **primaryHover** `#1245d0` — the darkened primary for hover/active on filled
  controls. Never used as a resting color.
- **ink** `#1a1a1a` / **inkStrong** `#111111` — body and heading text. `inkStrong`
  is reserved for the highest-emphasis text (active anchor labels, page headings).
- **muted** `#888888` — secondary text: timestamps, counts, the footer, disabled
  states. Deliberately low-contrast so it recedes.
- **surface** `#ffffff` — the toolbar, pills, modals. The overlay floats on white
  regardless of the document's own background.
- **surfaceSubtle** `#f5f6f8` — hover fills and inset panels within the chrome.
- **border** `#e5e5e7` — hairline dividers (toolbar bottom edge, pill outlines).
  1px, never heavier.
- **highlight** `#fff7d0` — the warm amber wash on commented text
  (`box-decoration-break: clone`, so it wraps line breaks cleanly). This is the
  one color that touches the document body, and it exists solely to say "there is
  a comment here."
- **highlightActive** `#ffd84d` — the same highlight, intensified, for the anchor
  the reader is currently focused on.
- **danger** `#cc3333` — destructive-action text only (delete/wipe in modals).
- **accent** `#3ecf8e` — a green success/confirmation tint (e.g. a fork/copy
  button flashing "done"). Transient feedback only, never a resting surface.

## Typography

One family everywhere: the **platform UI stack**
(`system-ui, -apple-system, "Segoe UI", Roboto, sans-serif`), with a **monospace
stack** (`ui-monospace, "SF Mono", Menlo, monospace`) for slugs, versions, and
code-like metadata. The overlay never ships a webfont — it borrows the OS font so
it feels native and adds zero load cost.

The ramp is compact because the chrome must stay small next to the document:

- **h1** `38px / 700` — the rendered doc's own title band.
- **h1-page** `30px / 700` — landing and catalog page headings.
- **body-md** `17px / 400` — readable body copy on pages.
- **ui** `13px / 400` and **ui-strong** `13px / 600` — the overlay's working size;
  toolbar labels, buttons, the FAB. 13px is the default for chrome.
- **label-sm** `12px / 400` — counts, timestamps, pill text.
- **mono** `13px` — slug/version identifiers.

## Layout

Spacing is an **6 / 8 / 12 / 16 / 20 px** scale. The overlay favors the smaller
end (`xs`–`md`) so controls sit tight; pages use `lg`–`xl` for section rhythm.

- The **toolbar** is a fixed 48px bar pinned to the top, `0 12px` inset, items
  gapped by `sm` (8px).
- The **FAB** sits fixed at `bottom: 16px; right: 16px` (`lg`).
- Filled buttons use `7px 14px` padding; the FAB uses `10px 16px`.
- Content on pages is centered in a readable measure with `xl` (20px) horizontal
  gutters.

## Elevation & Depth

Depth is **whisper-soft** — the chrome should feel laid *onto* the page, not
stacked in a z-tower of cards.

- **Toolbar**: `0 1px 2px rgba(0,0,0,0.02)` plus a 1px `border` bottom edge. Almost
  imperceptible; the border does most of the separation work.
- **FAB**: `0 4px 16px rgba(22,82,240,0.35)` — the one pronounced shadow, tinted
  with the primary blue so the float reads as an action, not a generic card.
- **Active anchor**: a soft `box-shadow` glow in the highlight color rather than a
  hard outline.

Z-index is layered high and deliberately (toolbar `999999`, FAB `999997`) so the
overlay always sits above arbitrary document stacking contexts.

The elevation scale is codified as tokens in `assets/overlay.js` (`--octo-shadow-*`):
`sm` (toolbar hairline lift), `card` (comment cards), `menu` (dropdowns, pickers,
the new-comment popup), `fab` (the one pronounced tinted float), and `active`
(the blue-tinted lift on a focused card/anchor). Use a token, not a raw `rgba()`.

**Focus.** A single `--octo-focus-ring` token (`0 0 0 3px rgba(22,82,240,0.15)`)
is the one focus affordance across the whole overlay. Text inputs (the comment
composer + reply box) show it on `:focus` alongside a `primary` border; every
interactive chrome control — bar buttons, chips, menu items, the FAB, the comment
pill, reaction/reply controls, modal buttons — shows it on `:focus-visible`, so
keyboard navigation is always visible without a ring flashing on mouse clicks.

## Shapes

A small, consistent radius set:

- **sm** `4px` — inset chips and small inputs.
- **md** `6px` — the default for buttons, menus, and modal panels.
- **lg** `10px` — larger surfaces (popovers, the comment composer).
- **xl** `12px` — the sign-in / publish / share modal panel.
- **pill** `999px` — the FAB and comment pills, which are fully rounded to read as
  distinct floating affordances rather than part of the boxy chrome. Circular
  avatars use `50%`.

## Components

- **bar** — the fixed top toolbar: white surface, ink text, 48px tall, 1px bottom
  border. Carries the Octo logo, the `slug / vN` version dropdown, and
  Copy/Fork/Share actions.
- **button-primary** — the filled action button: `{colors.primary}` on white text,
  `{rounded.md}`, `7px 14px`, weight 600. **button-primary-hover** darkens to
  `{colors.primaryHover}`.
- **fab** — the floating comment button, bottom-right, `{rounded.pill}`, primary
  blue with the tinted drop shadow. Shows the live comment count as a contrasting
  white pill badge (e.g. "💬 3").
- **anchor-mark** — the inline highlight on commented document text
  (`{colors.highlight}`); **anchor-mark-active** intensifies to
  `{colors.highlightActive}` for the focused anchor.
- **comment-pill** — the compact **"Comment"** affordance that floats on the
  top-right edge of a hovered element: primary blue, `{rounded.pill}`, icon +
  label, ~25px tall. Every box property is reset with `!important` so document CSS
  (e.g. an `svg { margin }` rule) can't distort it — the overlay floats over
  untrusted HTML and must be fully insulated.
- **comment-composer** — the new-comment popup and the reply box: a light
  `{colors.surface}` panel (matching the rest of the chrome, not a dark sheet), a
  muted anchor-preview chip, a real close button with a hit area, and a textarea
  that rests on a `{colors.border}` outline and gains a `primary` border +
  `--octo-focus-ring` on focus.
- **table** — minimal divider style: no cell fills or per-cell borders, a hairline
  (`{colors.border}`) under each row, a slightly stronger rule (`{colors.quoteRule}`)
  under the uppercase muted header, and a `{colors.surfaceSubtle}` row hover.
  `border-collapse: collapse` so it stays clean at any width.

## Do's and Don'ts

- **Do** use `{colors.primary}` for *every* interactive affordance — one blue is
  the entire action vocabulary. **Don't** introduce a second action color.
- **Do** keep `{colors.highlight}` / `{colors.highlightActive}` the *only* colors
  that touch the rendered document body. **Don't** let overlay surfaces bleed
  color onto user content.
- **Do** stay on the platform UI font at 12–13px for chrome. **Don't** add a
  webfont or bump chrome type — the overlay must read as native browser UI.
- **Do** keep elevation soft (hairline borders + faint shadows); reserve the one
  pronounced shadow for the FAB. **Don't** stack heavy card shadows in the chrome.
- **Do** use `{colors.danger}` for destructive text only, and `{colors.accent}`
  for transient success feedback only. **Don't** use either as a resting surface.
- **Do** use `pill` radius exclusively for floating affordances (FAB, pills) and
  `md` for everything else. **Don't** mix radii within one control group.
- **Do** give every interactive control a `--octo-focus-ring` on `:focus-visible`,
  and use `{colors.mutedDark}` (not `{colors.muted}`) for text at 12px or smaller so
  it clears WCAG AA. **Don't** ship chrome that's invisible to keyboard users or
  fails contrast at small sizes.
- **Do** fully reset a chrome element's box with `!important` when it floats over
  the document (the comment pill), and give document prose `overflow-wrap: anywhere`
  so a long URL can't force a horizontal scrollbar. **Don't** assume the document's
  own CSS won't leak into overlay chrome — it will.
