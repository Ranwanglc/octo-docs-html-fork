# Comment anchoring

How comments bind to a doc, and how to interpret them when editing.

Comments are persisted with one of two anchor shapes:

```json
// text anchor
{ "id": "c_<ts>", "version": 1, "text": "what the user wrote",
  "status": "open", "created_at": "<iso>",
  "anchor": { "kind": "text", "text": "exact highlighted text",
              "context_before": "...", "context_after": "..." } }

// element (artifact) anchor — IDENTITY-BASED
{ "id": "c_<ts>", "version": 1, "text": "what the user wrote",
  "status": "open", "created_at": "<iso>",
  "anchor": { "kind": "element",
              "aid": "<content-hash>",        // primary key: the server-stamped
                                              //   data-odoc-aid on the artifact.
                                              //   Same artifact across versions = same aid.
              "selector": "[data-odoc-aid=\"...\"]",  // mirror of aid; legacy
                                                       // comments may still carry
                                                       // a positional selector.
              "label": "svg",                 // tag hint
              "fingerprint": { ... },         // legacy content fingerprint
              "fallback": { "ratio": ..., "nearestHeading": ... } } }

// lost-anchor — publish-time reconciliation marks an element comment lost when
// its aid disappears or can't be resolved unambiguously. Renders "unanchored";
// never silently re-attached.
{ ..., "anchor": { "kind": "lost", "reason": "aid not found in version" } }
```

**Text anchors:** find the anchor text in the current HTML and apply the change.
`context_before` / `context_after` (~60 chars each side) disambiguate when the
same text appears multiple times. If the text no longer exists, apply as a
general directive.

**Element anchors:** identity is the **`aid`** — the server auto-stamps
`data-odoc-aid="<content-hash>"` on every commentable artifact at publish time and
reconciles existing anchors against the new artifact set on every upload. You
don't preserve ids manually; just regenerate the doc naturally. Comments on
unchanged artifacts stay anchored; comments on artifacts you genuinely replaced go
`kind: "lost"` automatically.

Pull comments with `scripts/octo.sh pull <slug>` (or
`GET /v1/comments?slug=<slug>&version=all`). The wire timestamp field is
`created_at`.

See [authoring.md](./authoring.md#comment-anchor-stability-important-when-editing)
for how to keep artifacts stable across versions.
