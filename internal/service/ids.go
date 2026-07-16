// Package service holds the application logic layer: document publishing,
// comment mutation (serialized per slug), and authentication. Routes call these;
// services own no HTTP concerns and depend only on storage interfaces and core.
package service

import (
	"crypto/rand"
	"encoding/hex"
)

// randHex returns n random bytes as a hex string (2n chars).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is catastrophic and not recoverable here.
		panic("service: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// NewToken returns a fresh opaque write token (256 bits of entropy).
func NewToken() string { return randHex(32) }

// NewSessionID returns a fresh session id.
func NewSessionID() string { return randHex(24) }

// NewShareCode returns a fresh per-doc share code (128 bits of entropy). It is a
// capability: anyone holding it gets read+comment access to that one doc.
func NewShareCode() string { return randHex(16) }

// newCommentID / newReplyID mirror the upstream id formats (prefix + time + rand)
// closely enough for uniqueness; exact byte format is not part of any contract.
func newCommentID(now string) string { return "c_" + compactTime(now) + "_" + randHex(4) }
func newReplyID(now string) string   { return "r_" + compactTime(now) + "_" + randHex(4) }

// compactTime strips an ISO timestamp to digits for a compact id segment.
func compactTime(iso string) string {
	out := make([]byte, 0, len(iso))
	for i := 0; i < len(iso); i++ {
		if iso[i] >= '0' && iso[i] <= '9' {
			out = append(out, iso[i])
		}
	}
	return string(out)
}
