// Package core is the dependency-free domain kernel: artifact identity stamping,
// the comment event-log fold, mutation ops, and anchor reconciliation.
//
// It is a verbatim-equivalent port of the original TypeScript logic.
// The same input MUST produce byte-identical output (for stamping) or
// logically-identical snapshots (for the fold), pinned by the tests in
// internal/core/*_test.go.
package core

import (
	"strconv"
	"unicode/utf16"
)

// Cyrb53 is a 53-bit public-domain string hash. It must match the JavaScript
// implementation in both the server and the browser overlay bit-for-bit so that
// identities computed on either side agree.
//
// Three traps the JS original imposes, reproduced exactly here:
//
//  1. Math.imul semantics: 32-bit wrap-around integer multiply. We use uint32
//     arithmetic, which wraps identically.
//  2. charCodeAt iterates UTF-16 CODE UNITS, not bytes and not runes. A Go
//     string ranges over runes and is stored as UTF-8, so we must first encode
//     to UTF-16 ([]uint16) and iterate that. CJK, emoji, and astral-plane
//     characters hash differently under each scheme — this is the subtle bug.
//  3. The final mix assembles a 53-bit value as a float64
//     (4294967296*(2097151&h2) + (h1>>>0)) then base36-encodes it.
func Cyrb53(s string, seed uint32) string {
	h1 := uint32(0xdeadbeef) ^ seed
	h2 := uint32(0x41c6ce57) ^ seed

	for _, ch := range utf16.Encode([]rune(s)) {
		c := uint32(ch)
		h1 = imul(h1^c, 2654435761)
		h2 = imul(h2^c, 1597334677)
	}

	h1 = imul(h1^(h1>>16), 2246822507) ^ imul(h2^(h2>>13), 3266489909)
	h2 = imul(h2^(h2>>16), 2246822507) ^ imul(h1^(h1>>13), 3266489909)

	// 4294967296 * (2097151 & h2) + (h1 >>> 0), computed exactly. The result is
	// < 2^53 so it is representable; we keep it in uint64 and base36-encode to
	// match Number.prototype.toString(36).
	v := uint64(2097151&h2)*4294967296 + uint64(h1)
	return strconv.FormatUint(v, 36)
}

// imul reproduces JavaScript's Math.imul: C-style 32-bit integer multiplication
// with wrap-around. uint32 multiplication in Go wraps modulo 2^32 identically.
func imul(a, b uint32) uint32 {
	return a * b
}
