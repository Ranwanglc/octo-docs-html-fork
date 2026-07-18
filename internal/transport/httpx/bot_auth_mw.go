package httpx

import (
	"context"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/octoidentity"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// botSessionCtxKey marks sessions verified by BotAuth, separate from trust-header
// sessions on octoSessionCtxKey which are not sufficient for publish auth.
type botSessionCtxKey struct{}

// botTokenCtxKey carries the publishing bot's own bearer token so the async
// docs-backend registration can authenticate as that bot (registering the doc
// under the publisher, not a fixed process identity).
type botTokenCtxKey struct{}

// botAuthMiddleware enriches Bearer bot tokens into the same context session
// used by proxy trust headers. It is intentionally non-gating so legacy bearer
// credentials continue through the existing capability chain.
func (s *Server) botAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg == nil || !s.cfg.BotAuthEnabled || octoSessionFromCtx(r.Context()) != nil {
			next.ServeHTTP(w, r)
			return
		}
		token := bearerToken(r)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		provider, err := octoidentity.Get()
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		bi, err := provider.VerifyBot(r.Context(), token)
		if err != nil || bi == nil {
			next.ServeHTTP(w, r)
			return
		}
		name := bi.Name
		if name == "" {
			name = bi.UID
		}
		sess := &storage.Session{
			Login: bi.UID,
			Name:  name,
			// A bot is NOT a global superAdmin. Author capability comes from the
			// bot's OwnerUID matching the doc's creator_uid (see bestCred), not from
			// a blanket role — otherwise every bot could write every doc.
			OwnerUID: bi.OwnerUID,
			Created:  time.Now().UTC().Format(time.RFC3339),
		}
		ctx := context.WithValue(r.Context(), octoSessionCtxKey{}, sess)
		ctx = context.WithValue(ctx, botSessionCtxKey{}, sess)
		ctx = context.WithValue(ctx, botTokenCtxKey{}, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func botSessionFromCtx(ctx context.Context) *storage.Session {
	if v, ok := ctx.Value(botSessionCtxKey{}).(*storage.Session); ok {
		return v
	}
	return nil
}

// botTokenFromCtx returns the publishing bot's own bearer token stashed by
// botAuthMiddleware, or "" when the request was not bot-authenticated.
func botTokenFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(botTokenCtxKey{}).(string); ok {
		return v
	}
	return ""
}
