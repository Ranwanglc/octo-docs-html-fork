// Package assets embeds static browser-side resources served verbatim by the
// server. overlay.js is the byte-equivalent browser overlay (text selection,
// commenting UI, anchoring); it is injected into rendered documents and is the
// single source of truth — never transpiled.
package assets

import _ "embed"

// OverlayJS is the browser overlay source, injected verbatim into rendered docs.
//
//go:embed overlay.js
var OverlayJS string
