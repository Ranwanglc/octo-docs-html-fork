package httpx

import (
	"context"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/octoidentity"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// verifyUserToken fills an octo session from a raw user token when the reverse
// proxy did not already supply trust-header identity. It is the direct-hit path:
// a browser/CLI reaching doc without docs_proxy carries its octo token, which we
// exchange with octo-server's /v1/auth/verify for a trusted uid.
//
// Non-gating by design. Trust headers win (octoIdentityMiddleware already ran),
// so a present session is never overwritten. A missing/invalid token, a disabled
// provider, or an unreachable upstream all fall through silently — bestCred then
// decides access (and 404s an unauthorised caller). This keeps identity purely
// additive: verify only ever adds a session, never blocks a request.
func (s *Server) verifyUserToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Trust-header (or bot) identity already resolved ⇒ do not re-verify or
		// overwrite; the proxy path is authoritative.
		if octoSessionFromCtx(r.Context()) != nil {
			next.ServeHTTP(w, r)
			return
		}
		tok := userToken(r)
		if tok == "" {
			next.ServeHTTP(w, r)
			return
		}
		provider, err := octoidentity.Get()
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		u, err := provider.VerifyToken(r.Context(), tok)
		if err != nil || u == nil || u.UID == "" {
			next.ServeHTTP(w, r)
			return
		}
		name := u.Name
		if name == "" {
			name = u.UID
		}
		sess := &storage.Session{
			Login:   u.UID,
			Name:    name,
			Role:    u.Role,
			Created: time.Now().UTC().Format(time.RFC3339),
		}
		ctx := context.WithValue(r.Context(), octoSessionCtxKey{}, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// userToken reads the caller's raw octo token: the dedicated "token" header
// first (the octo client convention), falling back to Authorization: Bearer so
// CLI callers carrying a Bearer token are covered too.
func userToken(r *http.Request) string {
	if t := r.Header.Get("token"); t != "" {
		return t
	}
	return bearerToken(r)
}
