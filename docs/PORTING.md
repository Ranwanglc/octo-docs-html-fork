# Porting notes: TypeScript → Go

octo-doc's domain kernel (`internal/core`) is a verbatim-equivalent port of the
original TypeScript implementation. The success criterion
is **byte-equivalence**: the same input HTML must produce the same stamped output,
and the same event log must fold to the same snapshot. This document records how
that equivalence is guaranteed and the subtle traps the port had to clear.

## The parity safety net

While the TypeScript still existed, a generator ran the *original* TS functions
over a battery of inputs and their outputs were baked into the Go tests as the
parity baseline. Those exact expectations now live directly in
`internal/core/*_test.go` as pinned literals — the external `testdata/golden`
fixtures were removed once the port was complete and the TypeScript deleted.

The tests in `internal/core/*_test.go` assert:

- **Byte-equivalence** for `StampAids` and `Cyrb53` (pure, fully deterministic) —
  exact stamped HTML, exact aid strings, exact base-36 hashes.
- **Behavioral equivalence** for the fold/ops/reconcile paths — folded snapshot
  fields, op status codes, anchor rebinding — driven through the public API
  (`ApplyCommentOp` → `SnapshotList`/`HistoryList`, `ReconcileAnchors`). One-shot
  event ids (`EventEID`) are intentionally non-deterministic but never appear in a
  folded snapshot, so snapshots stay stable.

The pinned expectations are a one-time parity baseline: `core` logic is considered
stable, so changing an output is a deliberate act — update the corresponding
`_test.go` literal in lockstep.

## The four traps

These are the places where a naive Go translation would silently diverge. Each
has dedicated test coverage.

### 1. `Math.imul` — 32-bit wrap-around multiply

`cyrb53` multiplies with JavaScript's `Math.imul`, which is C-style 32-bit
integer multiplication that wraps modulo 2³². Go's `uint32` multiplication wraps
identically, so `imul(a, b uint32) uint32 { return a * b }` is exact. The final
mix assembles a 53-bit value (`4294967296*(2097151&h2) + (h1>>>0)`) — computed in
`uint64` and base36-encoded to match `Number.prototype.toString(36)`.

### 2. `charCodeAt` — UTF-16 code units, not bytes or runes

`cyrb53` iterates `str.length` via `charCodeAt(i)`, i.e. **UTF-16 code units**. A
Go `string` is UTF-8 bytes and `range` yields runes — both differ from UTF-16 for
any non-ASCII text. The port first encodes to `[]uint16` with
`utf16.Encode([]rune(s))` and iterates that. Test vectors include CJK, emoji, and
astral-plane characters (surrogate pairs) to lock this down.

### 3. RE2 has no backreferences

The TS heading scanner used `</h\1>` (a backreference), which Go's `regexp` (RE2)
cannot compile. `collectHeadings` instead loops the three heading levels and
pairs open/close tags by hand. Several other `stamp.ts` regexes used a manual
`lastIndex` cursor; these became explicit scanning loops in `stamp.go`.

A bonus invariant makes the offset arithmetic safe: every structural delimiter
`StampAids` keys on (`<`, `>`, tag names, attribute quotes) is ASCII, so byte
offsets land on the same boundaries JavaScript's UTF-16 offsets would. Sliced
content is therefore identical bytes and hashes identically. The only place
UTF-16 length still matters is the 80-unit `head` excerpt, handled by
`utf16Slice`.

### 4. `\s` is ASCII in RE2, Unicode in JavaScript

JavaScript's regex `\s` matches the full Unicode whitespace set (vertical tab
`\v`/U+000B, no-break space U+00A0, ideographic space U+3000, the U+2000–200A
range, U+2028/U+2029, U+FEFF, …). Go's RE2 `\s` is **ASCII-only** (`[\t\n\f\r ]`).
`stamp.go` normalizes an artifact's innerHTML with a whitespace-collapse before
hashing it into the `data-odoc-aid`; a bare `\s+` would collapse ASCII runs but
leave a U+3000 or U+00A0 intact, producing a different normalized string — and a
different aid — than upstream for any document containing non-ASCII whitespace
(common in CJK source or pasted content). The port defines an explicit
JS-equivalent class (`jsSpace`/`wsClass` in `stamp.go`) and uses it in every
whitespace regex; `trimJSSpace` replaces `strings.TrimSpace` (whose
`unicode.IsSpace` set also differs: it includes U+0085 NEL, which JS `.trim()`
does not, and historically excluded U+FEFF). Covered by the stamp Unicode-whitespace test.

## Deliberate divergences

- **`SafeJSONForScript` restores U+2028/U+2029.** Go's `encoding/json` always
  escapes the line/paragraph separators to ` `/` ` even with
  `SetEscapeHTML(false)`, whereas JS `JSON.stringify` emits them raw. The overlay
  config is injected as a JSON literal inside `<script>window.__ODOC__ = …`, so to
  keep those bytes identical to upstream the port un-escapes them back to the raw
  code points after marshaling. (Safe here: they are hazardous only in a *bare* JS
  string literal, not in a JSON value inside a script element.)
- **`EventEID` for one-shot events.** Upstream used `Math.random()`; the Go port
  uses an atomic counter + high-resolution time. This only affects the uniqueness
  suffix of non-idempotent event ids, never the fold result (`DedupEvents` keys on
  the id; idempotent events keep their deterministic ids).
- **Storage is PostgreSQL + S3 only.** Metadata lives in `MetadataStore`
  (pgx/JSONB) and blobs in `BlobStore` (aws-sdk-go-v2). The SQLite+FS
  reference adapters were not ported.
