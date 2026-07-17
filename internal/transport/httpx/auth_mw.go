package httpx

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
)

// requireWriteOrBotOwnerAuth gates publish, whose slug is in the request body.
// Any authenticated octo identity may create/publish a doc; the creator uid is
// stamped on first publish and governs later author-only ops. Write tokens are
// no longer an auth source here.
func (s *Server) requireWriteOrBotOwnerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sess := botSessionFromCtx(r.Context()); sess != nil && sess.Login != "" {
			next.ServeHTTP(w, r)
			return
		}
		if sess := octoSessionFromCtx(r.Context()); sess != nil && sess.Login != "" {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, s.logger, apperr.Unauthorized("", ""))
	})
}
