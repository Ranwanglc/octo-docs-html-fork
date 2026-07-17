package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
)

// Server holds the dependencies shared by all handlers.
type Server struct {
	cfg      *config.Config
	logger   *slog.Logger
	docs     *service.DocService
	comments *service.CommentService
	assets   *service.AssetService
	auth     *service.AuthService
	// docBinding queries octo-server's per-slug binding to derive a per-uid cap
	// (FEAT-3). Nil = channel disabled (unset OCTO_DOC_BINDING_URL); bestCred
	// then falls back to trust-header identity + share-code semantics.
	docBinding *service.DocBindingClient
	overlayJS  string
	health     func(context.Context) error
}

// Deps bundles the constructor arguments for a Server.
type Deps struct {
	Config   *config.Config
	Logger   *slog.Logger
	Docs     *service.DocService
	Comments *service.CommentService
	Assets   *service.AssetService
	Auth     *service.AuthService
	// DocBinding is optional. Nil = FEAT-3 doc_binding channel disabled;
	// bestCred skips the binding probe.
	DocBinding *service.DocBindingClient
	OverlayJS  string
	// Health verifies backing stores are reachable (readiness probe). Optional; a
	// nil Health means /healthz reports liveness only.
	Health func(context.Context) error
}

// New constructs a Server.
func New(d Deps) *Server {
	return &Server{
		cfg:        d.Config,
		logger:     d.Logger,
		docs:       d.Docs,
		comments:   d.Comments,
		assets:     d.Assets,
		auth:       d.Auth,
		docBinding: d.DocBinding,
		overlayJS:  d.OverlayJS,
		health:     d.Health,
	}
}

// Handler builds the HTTP handler with all routes and middleware wired.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()

	writeLimiter := newRateLimiter(s.cfg.RateLimitWindow, s.cfg.RateLimitMax)

	// Container liveness probe — not a versioned REST resource, stays at root.
	r.Get("/healthz", s.handleHealthz)

	// All JSON APIs live under /v1 (the single current API version). Handlers
	// emit the {data}/{error} envelope; the chi mount provides the /v1 prefix.
	r.Route("/v1", func(r chi.Router) {
		// Health + identity.
		r.Get("/ping", s.handlePing)

		// Admin / auth. Viewer identity in fusion arrives via internal trust
		// headers; /auth/me reports the resolved identity and logout clears any
		// legacy cookie session.
		r.Post("/admin/bootstrap", s.cors(s.wrap(s.handleBootstrap)))
		r.Get("/auth/me", s.wrap(s.handleAuthMe))
		r.Post("/auth/login", s.cors(s.wrap(s.handleLogin)))
		r.Post("/auth/logout", s.cors(s.wrap(s.handleLogout)))

		// Documents.
		r.With(s.requireWriteOrBotOwnerAuth).Method(http.MethodPost, "/docs", s.cors(s.limit(writeLimiter, false, s.wrap(s.handlePublish))))
		// Owner-scope doc index; owner check lives inside handleListDocs (same style as /me and /auth/me).
		r.Get("/docs", s.cors(s.wrap(s.handleListDocs)))
		r.Get("/docs/{slug}", s.cors(s.requireDocReadJSON(slugFromPath, s.wrap(s.handleGetDoc))))
		r.Get("/docs/{slug}/versions", s.cors(s.requireDocReadJSON(slugFromPath, s.wrap(s.handleVersions))))
		r.With(s.requireDocAuthor).Delete("/docs/{slug}", s.cors(s.wrap(s.handleDeleteDoc)))
		// Draft slot. Draft-first creation must work before any version exists, so
		// these use requireDocAuthorOrFirstCreate: any authenticated session may
		// create a brand-new slug (creator stamped on that write); once owned it is
		// strict author-only. Author is accepted via Bearer (CLI) or per-doc cookie.
		r.With(s.requireDocAuthorOrFirstCreate).Method(http.MethodPut, "/docs/{slug}/draft", s.cors(s.limit(writeLimiter, false, s.wrap(s.handleSaveDraft))))
		r.With(s.requireDocAuthorOrFirstCreate).Post("/docs/{slug}/draft/promote", s.cors(s.limit(writeLimiter, false, s.wrap(s.handlePromote))))
		// Share: mint / rotate / revoke the per-doc read+comment code (author-only).
		r.With(s.requireDocAuthor).Post("/docs/{slug}/share", s.cors(s.wrap(s.handleShare)))
		r.With(s.requireDocAuthor).Delete("/docs/{slug}/share", s.cors(s.wrap(s.handleRevokeShare)))

		// Per-uid access grants (member management): author lists/grants/revokes;
		// a granted uid resolves to reader in bestCred.
		r.With(s.requireDocAuthor).Get("/docs/{slug}/grants", s.cors(s.wrap(s.handleListGrants)))
		r.With(s.requireDocAuthor).Put("/docs/{slug}/grants", s.cors(s.wrap(s.handleAddGrant)))
		r.With(s.requireDocAuthor).Delete("/docs/{slug}/grants/{uid}", s.cors(s.wrap(s.handleRemoveGrant)))

		// Media assets: author uploads/deletes; a reader (share code) may list.
		// The raw bytes are served from the /d/ tree below, under the same reader
		// gate as a version render.
		r.With(s.requireDocAuthor).Method(http.MethodPost, "/docs/{slug}/assets", s.cors(s.limit(writeLimiter, false, s.wrap(s.handleUploadAsset))))
		r.Get("/docs/{slug}/assets", s.cors(s.requireDocReadJSON(slugFromPath, s.wrap(s.handleListAssets))))
		r.With(s.requireDocAuthor).Delete("/docs/{slug}/assets/{sha256}", s.cors(s.wrap(s.handleDeleteAsset)))

		// Comments + reactions. Reads and writes require at least a reader
		// capability (the doc's share code) — enforced per-handler since the slug
		// arrives in the body on POST/PATCH.
		r.Get("/comments", s.cors(s.requireDocReadJSON(slugFromQuery, s.limit(writeLimiter, true, s.wrap(s.handleListComments)))))
		r.Post("/comments", s.cors(s.limit(writeLimiter, true, s.wrap(s.handleCreateComment))))
		r.Patch("/comments", s.cors(s.limit(writeLimiter, true, s.wrap(s.handlePatchComment))))
		r.Delete("/comments", s.cors(s.limit(writeLimiter, true, s.wrap(s.handleDeleteComment))))
		r.Post("/reactions", s.cors(s.limit(writeLimiter, false, s.wrap(s.handleReact))))
		r.Post("/agent/replies", s.cors(s.limit(writeLimiter, false, s.wrap(s.handleAgentReply))))
		// Per-aid single-element read/replace for agents (author-gated by creator
		// uid inside the handler, since the slug arrives in the body). Get is
		// read-only; Replace republishes a new version (rate-limited like publish).
		r.Post("/agent/element/get", s.cors(s.wrap(s.handleAgentElementGet)))
		r.Post("/agent/element/replace", s.cors(s.limit(writeLimiter, false, s.wrap(s.handleAgentElementReplace))))
	})

	// Rendered docs + export/fork. Default-private: a reader needs the doc's share
	// code (via ?code= → cookie), the author needs the write token. The draft view
	// is author-only; the write token can arrive as ?code= (browser) and is
	// exchanged for a cookie the same way a reader code is.
	r.Get("/d/{slug}/draft", s.requireDocAuthorHTML(s.secHeaders(s.wrap(s.handleRenderDraft))))
	r.Head("/d/{slug}/draft", s.requireDocAuthorHTML(s.secHeaders(s.wrap(s.handleRenderDraft))))
	r.Get("/d/{slug}/v/{version}", s.requireDocReadHTML(s.secHeaders(s.wrap(s.handleRender))))
	r.Head("/d/{slug}/v/{version}", s.requireDocReadHTML(s.secHeaders(s.wrap(s.handleRender))))
	r.Get("/d/{slug}/v/{version}/{kind}", s.requireDocReadHTML(s.secHeaders(s.wrap(s.handleForkExport))))

	// Raw media assets referenced by a doc's HTML. Reader-gated like a render; the
	// handler sets its own locked-down CSP (not docSecurityHeaders), so no
	// secHeaders wrapper here.
	r.Get("/d/{slug}/assets/{sha256}", s.requireAssetRead(s.wrap(s.handleServeAsset)))
	r.Head("/d/{slug}/assets/{sha256}", s.requireAssetRead(s.wrap(s.handleServeAsset)))

	// Pages (browser HTML).
	r.Get("/", s.handleLanding)
	r.Get("/me", s.wrap(s.handleCatalog))

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Not found", http.StatusNotFound)
	})
	return middleware.RequestID(s.octoIdentityMiddleware(s.botAuthMiddleware(s.verifyUserToken(s.accessLog(r)))))
}

// handlerFunc is a handler that may return an error, mapped centrally.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// wrap adapts a handlerFunc into an http.HandlerFunc, routing errors to writeErr.
func (s *Server) wrap(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			writeErr(w, s.logger, err)
		}
	}
}

// cors adds CORS headers for the API. Reads (GET, and OPTIONS preflights for a
// GET) are allowed from any origin, since reads are safe/idempotent and used by
// CLIs and agents. Mutating requests — and preflights for them — echo the request
// origin only if it is in the configured CORSOrigins allowlist; with no allowlist,
// no ACAO is sent on writes, so a browser blocks cross-origin mutations
// (same-origin still works).
func (s *Server) cors(next http.HandlerFunc) http.HandlerFunc {
	allowed := map[string]struct{}{}
	for _, o := range s.cfg.CORSOrigins {
		allowed[o] = struct{}{}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Classify by the effective method: for a preflight, that is the method the
		// real request will use (Access-Control-Request-Method), not OPTIONS itself —
		// otherwise a write preflight would be treated as a read and wrongly get *.
		effective := r.Method
		if r.Method == http.MethodOptions {
			if reqMethod := r.Header.Get("Access-Control-Request-Method"); reqMethod != "" {
				effective = reqMethod
			}
		}
		isRead := effective == http.MethodGet || effective == http.MethodHead
		switch {
		case isRead:
			w.Header().Set("Access-Control-Allow-Origin", "*")
		case origin != "":
			if _, ok := allowed[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// secHeaders attaches the document security headers to /d/* responses.
func (s *Server) secHeaders(next http.HandlerFunc) http.HandlerFunc {
	headers := docSecurityHeaders(s.cfg.FrameAncestors)
	return func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		next(w, r)
	}
}

// docSecurityHeaders builds the CSP + framing headers for rendered documents.
//
// media-src / frame-src / object-src are listed explicitly rather than left to
// fall back to default-src: prompt-native docs routinely embed <video>/<audio>,
// third-party <iframe>s (e.g. video embeds), and self-hosted assets, and an
// implicit fallback would silently break those the moment default-src is
// tightened. Keeping them explicit makes the media capability intentional and
// independently adjustable.
func docSecurityHeaders(frameAncestors string) map[string]string {
	csp := strings.Join([]string{
		"default-src 'self' data: blob: https:",
		"script-src 'self' 'unsafe-inline' 'unsafe-eval' data: blob: https:",
		"style-src 'self' 'unsafe-inline' https:",
		"img-src 'self' data: blob: https:",
		"media-src 'self' data: blob: https:",
		"font-src 'self' data: https:",
		"connect-src 'self' https:",
		"frame-src 'self' https:",
		"object-src 'self' data: blob: https:",
		"base-uri 'self'",
		"frame-ancestors " + frameAncestors,
	}, "; ")
	xfo := "SAMEORIGIN"
	if frameAncestors == "'none'" {
		xfo = "DENY"
	}
	return map[string]string{
		"Content-Security-Policy": csp,
		"X-Frame-Options":         xfo,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
	}
}
