package httpx

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// Identity sources on a doc request (in priority order at bestCred time):
//
//  1. Internal trust headers (OCT-145 方案 C, this middleware): the reverse
//     proxy (octo-server docs_proxy) has already authenticated the caller and
//     forwards X-Octo-Uid/Name/Role. doc only listens on the internal network,
//     so these three headers are trusted verbatim — no userinfo round trip.
//  2. Doc creator match (creator_uid == user Login / bot OwnerUID) or octo
//     superAdmin → CapAuthor via bestCred (capability.go).
//  3. Per-doc share code (Bearer / cookie / ?code=) → CapReader.
//  4. Nothing → 404 (hidden-existence, never 403).
//
// The middleware only builds and stashes the octo-derived session on the
// request context; capability.go decides how to grant caps from it. The session
// is in-memory only (context-scoped, never persisted, no cookie emitted) — that
// is the "session grant, not cookie" invariant from OCT-133.

// Trust header names. Reverse proxy contract (OCT-147 docs_proxy) guarantees
// X-Octo-Role ∈ {"superAdmin","admin","member"} (unknown/empty is downgraded
// to "member" upstream), so we do not re-normalize here.
const (
	octoUIDHeader   = "X-Octo-Uid"
	octoNameHeader  = "X-Octo-Name"
	octoRoleHeader  = "X-Octo-Role"
	octoTokenHeader = "X-Octo-Token" // OCT-144 module token channel (unrelated to identity)
)

// octoSessionCtxKey stashes the resolved octo session. Unexported struct type so
// no other package can accidentally collide (Go context best practice).
type octoSessionCtxKey struct{}

// octoTokenCtxKey stashes the raw X-Octo-Token so the FEAT-3 doc_binding client
// can forward the same module token to octo-server without re-reading the
// header. Only set when the header is actually present. Never logged.
type octoTokenCtxKey struct{}

// octoIdentityMiddleware materialises the octo caller's identity onto the
// request context. Two independent inputs, both silent on absence:
//   - X-Octo-Uid/Name/Role → session (proxy-trusted, in-memory only).
//   - X-Octo-Token         → context stash for the doc_binding client.
//
// Both are enrichment, not gates — bestCred still decides. Wire-up is
// unconditional (see server.go); with zero octo headers the middleware is a
// two-map-lookup no-op.
func (s *Server) octoIdentityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if sess := sessionFromTrustedHeaders(r); sess != nil {
			ctx = context.WithValue(ctx, octoSessionCtxKey{}, sess)
		}
		if tok := strings.TrimSpace(r.Header.Get(octoTokenHeader)); tok != "" {
			ctx = context.WithValue(ctx, octoTokenCtxKey{}, tok)
		}
		if ctx != r.Context() {
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// sessionFromTrustedHeaders builds an octo session from the three proxy-signed
// identity headers. Empty uid ⇒ no session (uid is the only field callers key
// on; a session without a login would silently anonymise the caller).
func sessionFromTrustedHeaders(r *http.Request) *storage.Session {
	uid := strings.TrimSpace(r.Header.Get(octoUIDHeader))
	if uid == "" {
		return nil
	}
	name := strings.TrimSpace(r.Header.Get(octoNameHeader))
	if name == "" {
		name = uid
	}
	return &storage.Session{
		Login:   uid,
		Name:    name,
		Role:    strings.TrimSpace(r.Header.Get(octoRoleHeader)),
		Created: time.Now().UTC().Format(time.RFC3339),
	}
}

// octoSessionFromCtx returns the octo-derived session on the request context, or
// nil if the middleware saw no valid identity headers.
func octoSessionFromCtx(ctx context.Context) *storage.Session {
	if v, ok := ctx.Value(octoSessionCtxKey{}).(*storage.Session); ok {
		return v
	}
	return nil
}

// octoTokenFromCtx returns the raw X-Octo-Token stashed by the middleware, or
// "" if the request carried none. Used by the doc_binding client to forward the
// module token to octo-server. Never emit this to logs.
func octoTokenFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(octoTokenCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// resolveViewerSession is the single seam every handler uses to get the current
// viewer: octo identity wins over the legacy odoc_sid cookie so a logged-in
// octo user is never demoted by a stale local session. If neither is present,
// returns (nil, nil) — anonymous, same as today.
func (s *Server) resolveViewerSession(r *http.Request) (*storage.Session, error) {
	if sess := octoSessionFromCtx(r.Context()); sess != nil {
		return sess, nil
	}
	return s.auth.GetSession(r.Context(), sessionCookie(r))
}
