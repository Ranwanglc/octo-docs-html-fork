package httpx

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// Access control: every document is private by default. A credential grants a
// capability for a specific doc:
//   - author = the doc's creator uid matched (real user Login, or bot OwnerUID),
//     or an octo superAdmin. Full access.
//   - reader = a valid per-doc share code (Bearer, cookie, or ?code=). Read
//     published versions + comment/react. Never drafts/publish/promote/delete.
//   - none   → 404 (never confirm the doc exists).
//
// Browsers carry the code as ?code= on the first hit, which is exchanged for an
// HttpOnly cookie and redirected to a clean URL so the secret never lingers in
// history/logs/Referer. Agents/CLI carry it as Authorization: Bearer, so the same
// credential model works headless with no cookie.

// slugFromPath / slugFromQuery extract the slug for the read-JSON gate.
func slugFromPath(r *http.Request) string  { return chi.URLParam(r, "slug") }
func slugFromQuery(r *http.Request) string { return r.URL.Query().Get("slug") }

// capCookieName is the per-doc capability cookie. Scoping the name to the slug
// means one share link never leaks access to another doc. (The cookie's Path is
// "/" so it reaches /v1 routes too — see setCapCookie; only the name is scoped.)
func capCookieName(slug string) string { return "octo_cap_" + storage.HashSlug(slug) }

// credCandidates returns every credential a request presents for a doc, in no
// particular order: an Authorization Bearer (author write token or code-as-bearer,
// used by the CLI), the per-doc capability cookie, and the ?code= query param (a
// browser's first hit). A request can carry more than one — e.g. a browser holding
// a stale cookie that is then handed a freshly rotated ?code= link — so callers
// must resolve them all and take the strongest, never letting a weak/stale cookie
// mask a valid ?code= or Bearer.
func (s *Server) credCandidates(r *http.Request, slug string) []string {
	var creds []string
	if t := bearerToken(r); t != "" {
		creds = append(creds, t)
	}
	if c, err := r.Cookie(capCookieName(slug)); err == nil && c.Value != "" {
		creds = append(creds, c.Value)
	}
	if q := r.URL.Query().Get("code"); q != "" {
		creds = append(creds, q)
	}
	return creds
}

// resolveCap returns the highest capability any of the request's credentials
// grants for the slug. Resolving all candidates (rather than the first non-empty
// one) means a fresh valid ?code= or Bearer always wins over a stale cookie — so
// rotating a code cuts off the old link while a recipient's new link still works,
// and an author's ?code=<write-token> is honored even if the browser holds a
// weaker reader cookie for the same doc.
func (s *Server) resolveCap(r *http.Request, slug string) (service.Capability, error) {
	return s.bestCred(r, slug)
}

// bestCred returns the strongest capability any of the request's credentials or
// its octo session grants for the slug. The winning cred string is not returned:
// docHTMLGate validates the raw ?code= independently (so a stronger session
// grant does not suppress the clean-URL redirect and leak the code in
// history/Referer), and no other caller needs the string. If a future caller
// needs cookie provenance, thread the cred out again.
//
// FEAT-1 session→cap path (OCT-133): if an octo session is present and belongs
// to an octo superAdmin (Session.Role matches config), we upgrade to CapAuthor.
// Session grants belong to the session, not the URL, so they never surface as
// a per-doc cookie (docHTMLGate only cookies raw ?code= values).
//
// FEAT-3 doc_binding channel (OCT-143): if the caller is a non-superAdmin octo
// user, ask octo-server whether this uid can see the binding for the slug and,
// if so, whether they created it. hidden-404 / any error / no client wired ⇒
// skip this channel, preserving the FEAT-1 fallback. The probe runs only when
// (a) an octo session exists, (b) a doc_binding client is configured, and (c) a
// raw octo token was stashed on the context — otherwise we cannot forward the
// caller's identity to octo-server and any answer would be wrong.
func (s *Server) bestCred(r *http.Request, slug string) (service.Capability, error) {
	best := service.CapNone
	for _, cred := range s.credCandidates(r, slug) {
		cap, err := s.auth.CapabilityFor(r.Context(), slug, cred)
		if err != nil {
			return service.CapNone, err
		}
		if cap > best {
			best = cap
		}
	}
	sess := octoSessionFromCtx(r.Context())
	if sess != nil && s.auth.IsOwner(sess) {
		if service.CapAuthor > best {
			best = service.CapAuthor
		}
		return best, nil
	}
	// matchUID is the USER uid this session authors as. Two shapes:
	//   (a) a real octo user → its own Login is the author uid, or
	//   (b) a bot session → its OwnerUID (the user behind the bot); a bot's own
	//       Login is the bot uid and never matches a creator_uid.
	// Invariant: botAuthMiddleware stashes the SAME *Session under both
	// octoSessionCtxKey and botSessionCtxKey, so octoSessionFromCtx and
	// botSessionFromCtx here observe one identity — do not split them into
	// separate instances or the bot→OwnerUID mapping below silently breaks.
	matchUID := ""
	if sess != nil {
		if bs := botSessionFromCtx(r.Context()); bs != nil && bs.OwnerUID != "" {
			matchUID = bs.OwnerUID
		} else if sess.Login != "" {
			matchUID = sess.Login
		}
	}
	// Author-by-creator: the doc's creator uid is a USER uid, matched by matchUID
	// above. This replaces the removed "every bot is superAdmin" grant: a bot only
	// gets author on docs its owner created.
	if matchUID != "" {
		if meta, err := s.auth.MetaFor(r.Context(), slug); err != nil {
			return service.CapNone, err
		} else if meta != nil && meta.CreatorUID() != "" && meta.CreatorUID() == matchUID {
			if service.CapAuthor > best {
				best = service.CapAuthor
			}
			return best, nil
		}
	}
	// doc_grants: an explicitly granted USER uid gets reader (read+comment).
	// Reuse matchUID (already bot→OwnerUID resolved) so a granted user's bot
	// also reads. After author-by-creator so a grant never downgrades a creator.
	if matchUID != "" && best < service.CapReader {
		if meta, err := s.auth.MetaFor(r.Context(), slug); err != nil {
			return service.CapNone, err
		} else if meta != nil && meta.GrantRole(matchUID) != "" {
			best = service.CapReader
		}
	}
	// FEAT-3 doc_binding probe (see method comment). Only kicks in when we
	// actually have an octo session + a raw token to forward + a wired client.
	// A superAdmin already short-circuited above — the probe would give the
	// same or weaker answer, so we save the octo-server round trip.
	if sess != nil && s.docBinding != nil {
		if tok := octoTokenFromCtx(r.Context()); tok != "" {
			binding, err := s.docBinding.Resolve(r.Context(), tok, slug)
			if err != nil {
				// Flaky octo must not fail the request — log at debug and fall
				// through so share-code / write-token paths still work.
				if s.logger != nil {
					s.logger.Debug("doc_binding resolve failed", "err", err.Error())
				}
			} else if binding != nil {
				cap := service.CapReader
				// Match the doc_binding creator the same way as the author-by-creator
				// path above: matchUID resolves bot→OwnerUID, so a bot's owner is
				// recognized as creator (sess.Login alone would be the bot uid).
				if binding.CreatorUID != "" && matchUID != "" && binding.CreatorUID == matchUID {
					cap = service.CapAuthor
				}
				if cap > best {
					best = cap
				}
			}
		}
	}
	return best, nil
}

// capCtxKey stashes the resolved capability for handlers that branch on it.
// requireDocReadHTML gates an HTML /d/ route: it resolves the capability for the
// path {slug}, 404s on none, and — when the credential arrived as ?code= and is
// valid — sets the HttpOnly capability cookie and 302-redirects to the same URL
// without the query param (so the code leaves the address bar). Otherwise it
// continues to the handler.
func (s *Server) requireDocReadHTML(next http.HandlerFunc) http.HandlerFunc {
	return s.docHTMLGate(service.CapReader, next)
}

// requireDocAuthorHTML is the author-only HTML gate (draft view). It uses the same
// ?code= → cookie → 302 exchange, so the write token can arrive as ?code= in a
// browser (e.g. opening the draft with ?code=<write-token>) and then ride as a
// cookie — the only way a browser can present the author credential.
func (s *Server) requireDocAuthorHTML(next http.HandlerFunc) http.HandlerFunc {
	return s.docHTMLGate(service.CapAuthor, next)
}

// docHTMLGate resolves the capability for the path {slug}, requires at least min,
// performs the ?code=→cookie→302 exchange, else 404s (existence hidden).
func (s *Server) docHTMLGate(min service.Capability, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug, err := requireSlug(chi.URLParam(r, "slug"))
		if err != nil {
			writeErr(w, s.logger, err)
			return
		}
		cap, err := s.bestCred(r, slug)
		if err != nil {
			writeErr(w, s.logger, err)
			return
		}
		if cap < min {
			// Hide existence — same 404 the old PRIVATE gate returned.
			writeErr(w, s.logger, apperr.NotFound("Not found"))
			return
		}
		// Clean ?code= from the URL whenever the code itself is a valid doc
		// credential (share code OR write token), regardless of whether it's
		// what actually authorized this request. A stronger session grant
		// (octo superAdmin → CapAuthor) does not exempt us from stripping the
		// code — leaving it in the address bar leaks the reader/author secret
		// to history, Referer, and proxy logs. bearerToken guard keeps headless
		// clients (CLI carries the code as Bearer) out of the cookie path.
		// Session grants themselves never land in a cookie — they belong to
		// the session, not the URL — so we only cookie the raw ?code= value.
		if q := r.URL.Query().Get("code"); q != "" && bearerToken(r) == "" {
			qcap, err := s.auth.CapabilityFor(r.Context(), slug, q)
			if err != nil {
				writeErr(w, s.logger, err)
				return
			}
			if qcap >= service.CapReader {
				setCapCookie(w, slug, q, s.cfg.CookieSecure)
				clean := *r.URL
				cq := clean.Query()
				cq.Del("code")
				clean.RawQuery = cq.Encode()
				http.Redirect(w, r, clean.RequestURI(), http.StatusFound)
				return
			}
		}
		next(w, r)
	}
}

// requireDocReadJSON gates a JSON read route whose slug is a path or query param
// (versions, list-comments). No cookie/redirect — JSON clients (overlay via
// cookie, CLI via Bearer) present the credential directly.
func (s *Server) requireDocReadJSON(slugFrom func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug, err := requireSlug(slugFrom(r))
		if err != nil {
			writeErr(w, s.logger, err)
			return
		}
		cap, err := s.resolveCap(r, slug)
		if err != nil {
			writeErr(w, s.logger, err)
			return
		}
		if cap == service.CapNone {
			writeErr(w, s.logger, apperr.NotFound("Not found"))
			return
		}
		next(w, r)
	}
}

// requireDocCap resolves the capability for a body-slug mutation route (the slug
// is only known after the handler parses the body). Handlers call this once they
// have the slug; it returns a 404-worthy error on none. Returns nil when the
// caller has at least reader access.
func (s *Server) requireDocCap(r *http.Request, slug string) error {
	cap, err := s.resolveCap(r, slug)
	if err != nil {
		return err
	}
	if cap == service.CapNone {
		return apperr.NotFound("Not found")
	}
	return nil
}

// requireDocAuthorSlug enforces author capability for an explicit slug (used by
// body-slug routes like /agent/* where the slug is only known after the handler
// decodes the body, so it cannot ride path-based middleware). Returns a 404 on
// anything less than author, hiding both existence and the op.
func (s *Server) requireDocAuthorSlug(r *http.Request, slug string) error {
	cap, err := s.resolveCap(r, slug)
	if err != nil {
		return err
	}
	if cap != service.CapAuthor {
		return apperr.NotFound("Not found")
	}
	return nil
}

// requireDocAuthorOrFirstCreate gates draft-first mutations (draft save/promote)
// whose slug is in the path. Draft-first creation must work before any version
// exists, but only for a genuinely new slug: one with no stored creator AND no
// existing versions AND no existing draft. For such a slug any authenticated
// octo/bot session may create it (creator is stamped on that first write, same
// as publish).
//
// A pre-migration / write-token-era doc can have real versions or an existing
// draft while still carrying an empty creator_uid. Treating that as "no creator
// ⇒ first-create" would let any logged-in caller PUT /draft and stamp
// themselves as creator, hijacking someone else's existing doc as author. So the
// first-create bypass is restricted to slugs that carry no content at all; any
// creator-less doc that already has a version or a draft falls through to strict
// author-only (resolveCap → CapAuthor, only the superAdmin override can pass).
func (s *Server) requireDocAuthorOrFirstCreate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug, err := requireSlug(chi.URLParam(r, "slug"))
		if err != nil {
			writeErr(w, s.logger, err)
			return
		}
		meta, err := s.auth.MetaFor(r.Context(), slug)
		if err != nil {
			writeErr(w, s.logger, err)
			return
		}
		// First-create bypass: only a truly empty slug qualifies. meta==nil, or a
		// meta shell with no creator, no versions, and no draft. Any creator-less
		// doc that already has a version or a draft is existing content that must
		// not be claimable via the draft path — it goes to strict author-only.
		if meta == nil ||
			(meta.CreatorUID() == "" && len(meta.Versions) == 0 && !meta.HasDraft()) {
			if hasWriteSession(r.Context()) {
				next.ServeHTTP(w, r)
				return
			}
			writeErr(w, s.logger, apperr.NotFound("Not found"))
			return
		}
		// Doc already owned (or existing creator-less content) → strict author-only
		// (same as requireDocAuthor). A creator-less-but-existing doc can only be
		// authored by the superAdmin override; nobody can claim it via /draft.
		cap, err := s.resolveCap(r, slug)
		if err != nil {
			writeErr(w, s.logger, err)
			return
		}
		if cap != service.CapAuthor {
			writeErr(w, s.logger, apperr.NotFound("Not found"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hasWriteSession reports whether the request carries an authenticated session
// permitted to first-create a doc (a bot session or an octo-user session with a
// login). Mirrors requireWriteOrBotOwnerAuth's acceptance rule.
func hasWriteSession(ctx context.Context) bool {
	if bs := botSessionFromCtx(ctx); bs != nil && bs.Login != "" {
		return true
	}
	if sess := octoSessionFromCtx(ctx); sess != nil && sess.Login != "" {
		return true
	}
	return false
}

// requireDocAuthor is chi middleware for author-only mutations whose slug is in
// the path (share, draft save/promote, delete). It accepts the author credential
// via Bearer OR the per-doc cookie, so the overlay's Publish/Share buttons work
// in a browser (cookie) as well as the CLI (Bearer).
func (s *Server) requireDocAuthor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug, err := requireSlug(chi.URLParam(r, "slug"))
		if err != nil {
			writeErr(w, s.logger, err)
			return
		}
		cap, err := s.resolveCap(r, slug)
		if err != nil {
			writeErr(w, s.logger, err)
			return
		}
		if cap != service.CapAuthor {
			// A reader (or nobody) must not learn that author-only ops exist here.
			writeErr(w, s.logger, apperr.NotFound("Not found"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
