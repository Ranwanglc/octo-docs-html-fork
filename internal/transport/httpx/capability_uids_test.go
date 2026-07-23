package httpx

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// A2: sessionUIDs must split a caller into (selfUID, ownerUID) with the
// bot vs real-user semantics that A3’s three-tier author decision relies on.

func TestSessionUIDsRealUser(t *testing.T) {
	// A real octo user session: selfUID and ownerUID are both the real uid.
	ctx := context.WithValue(context.Background(), octoSessionCtxKey{},
		&storage.Session{Login: "u-real"})
	r := httptest.NewRequestWithContext(ctx, "GET", "/", nil)
	self, owner := sessionUIDs(r)
	if self != "u-real" || owner != "u-real" {
		t.Fatalf("real user sessionUIDs = (%q,%q); want (u-real,u-real)", self, owner)
	}
}

func TestSessionUIDsBotSession(t *testing.T) {
	// A bot session stashes the SAME *Session under both keys — selfUID must
	// be the bot uid (Login), ownerUID must be the owner uid (OwnerUID).
	sess := &storage.Session{Login: "b-bot", OwnerUID: "u-owner"}
	ctx := context.WithValue(context.Background(), octoSessionCtxKey{}, sess)
	ctx = context.WithValue(ctx, botSessionCtxKey{}, sess)
	r := httptest.NewRequestWithContext(ctx, "GET", "/", nil)
	self, owner := sessionUIDs(r)
	if self != "b-bot" || owner != "u-owner" {
		t.Fatalf("bot sessionUIDs = (%q,%q); want (b-bot,u-owner)", self, owner)
	}
}

func TestSessionUIDsAnonymous(t *testing.T) {
	// No session in context ⇒ both uids empty. bestCred relies on this to
	// skip the identity-driven tiers cleanly.
	r := httptest.NewRequest("GET", "/", nil)
	self, owner := sessionUIDs(r)
	if self != "" || owner != "" {
		t.Fatalf("anonymous sessionUIDs = (%q,%q); want empty", self, owner)
	}
}

func TestSessionUIDsBotWithoutOwner(t *testing.T) {
	// Defensive: a bot session with an empty OwnerUID must collapse to the
	// real-user shape (owner == self) rather than leaving ownerUID empty and
	// stripping the caller of any author tier.
	sess := &storage.Session{Login: "b-bot"}
	ctx := context.WithValue(context.Background(), octoSessionCtxKey{}, sess)
	ctx = context.WithValue(ctx, botSessionCtxKey{}, sess)
	r := httptest.NewRequestWithContext(ctx, "GET", "/", nil)
	self, owner := sessionUIDs(r)
	if self != "b-bot" || owner != "b-bot" {
		t.Fatalf("ownerless bot sessionUIDs = (%q,%q); want (b-bot,b-bot)", self, owner)
	}
}
