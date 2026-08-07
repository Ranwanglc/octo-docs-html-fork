package httpx_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/assets"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/log"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/transport/httpx"
)

// capCookie builds the per-doc capability cookie header value the way the server
// names it (octo_cap_<hashslug>), so tests can present a cookie credential.
func capCookie(slug, value string) string {
	return "octo_cap_" + storage.HashSlug(slug) + "=" + value
}

var errUnhealthy = errors.New("store down")

// newTestServerWithHealth builds a server whose /healthz uses the given check.
func newTestServerWithHealth(t *testing.T, check func() error) http.Handler {
	t.Helper()
	cfg := &config.Config{WriteToken: "t", MaxHTMLBytes: 1 << 20, RepoURL: "https://x", RateLimitMax: 0}
	store := memory.New()
	locker := sluglock.NewMemory()
	comments := service.NewCommentService(store, locker)
	docs := service.NewDocService(store, store, comments, locker, cfg.BaseURL, cfg.MaxHTMLBytes)
	auth := service.NewAuthService(store, cfg, locker)
	srv := httpx.New(httpx.Deps{
		Config: cfg, Logger: log.New("silent"), Docs: docs, Comments: comments, Auth: auth,
		OverlayJS: "/* overlay */",
		Health:    func(_ context.Context) error { return check() },
	})
	return srv.Handler()
}

// TestDocsPrivateByDefault verifies every doc is private by default: a caller
// with no credential gets 404 (existence hidden) on reads, the author (creator
// uid via trust headers) gets through, and a valid share code grants read-only access.
func TestDocsPrivateByDefault(t *testing.T) {
	h := newTestServer(t, nil) // default cfg
	auth := authorHdr()

	// Publish a doc (author = creator uid stamped from the trust-header session).
	// The doc is stored under the server-derived key, so address it by that key.
	pub := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"doc","html":"<html><body><p>hi</p></body></html>"}`)
	if pub.Code != http.StatusOK {
		t.Fatalf("setup publish = %d: %s", pub.Code, pub.Body.String())
	}
	key := pubKey(t, pub)

	// No credential → 404 on render, versions, and comments (existence hidden).
	for _, target := range []string{"/d/" + key + "/v/1", "/v1/docs/" + key + "/versions", "/v1/comments?slug=" + key} {
		if rec := do(t, h, http.MethodGet, target, nil, ""); rec.Code != http.StatusNotFound {
			t.Errorf("anonymous GET %s = %d; want 404 (private by default)", target, rec.Code)
		}
	}

	// Author (creator uid) reads everything.
	for _, target := range []string{"/d/" + key + "/v/1", "/v1/docs/" + key + "/versions", "/v1/comments?slug=" + key} {
		if rec := do(t, h, http.MethodGet, target, authorHdrNoCT(), ""); rec.Code == http.StatusNotFound {
			t.Errorf("author GET %s = 404; creator uid should grant read", target)
		}
	}

	// Mint a share code.
	sh := do(t, h, http.MethodPost, "/v1/docs/"+key+"/share", authorHdrNoCT(), "")
	if sh.Code != http.StatusOK {
		t.Fatalf("share = %d: %s", sh.Code, sh.Body.String())
	}
	var share map[string]any
	_ = json.Unmarshal(sh.Body.Bytes(), &share)
	code, _ := share["data"].(map[string]any)["code"].(string)
	if code == "" {
		t.Fatalf("share returned no code: %s", sh.Body.String())
	}

	// Reader (code as Bearer) reads published + list-comments, but a share code is
	// READ-ONLY: it can neither create comments nor read the draft (plan §1 — the
	// share code always has read capability only, never comment).
	codeAuth := map[string]string{"Authorization": "Bearer " + code}
	if rec := do(t, h, http.MethodGet, "/d/"+key+"/v/1", codeAuth, ""); rec.Code != http.StatusOK {
		t.Errorf("reader render with code = %d; want 200", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/v1/comments?slug="+key, codeAuth, ""); rec.Code != http.StatusOK {
		t.Errorf("reader list-comments with code = %d; want 200", rec.Code)
	}
	cm := do(t, h, http.MethodPost, "/v1/comments",
		map[string]string{"Authorization": "Bearer " + code, "Content-Type": "application/json"},
		`{"slug":"`+key+`","version":1,"text":"nice"}`)
	if cm.Code != http.StatusNotFound {
		t.Errorf("reader comment with code = %d; want 404 (share code is read-only): %s", cm.Code, cm.Body.String())
	}

	// A wrong code is rejected (404) on read and comment.
	bad := map[string]string{"Authorization": "Bearer deadbeefdeadbeef"}
	if rec := do(t, h, http.MethodGet, "/d/"+key+"/v/1", bad, ""); rec.Code != http.StatusNotFound {
		t.Errorf("wrong code render = %d; want 404", rec.Code)
	}

	// Rotating the code invalidates the old one.
	sh2 := do(t, h, http.MethodPost, "/v1/docs/"+key+"/share", authorHdrNoCT(), "")
	var share2 map[string]any
	_ = json.Unmarshal(sh2.Body.Bytes(), &share2)
	newCode, _ := share2["data"].(map[string]any)["code"].(string)
	if newCode == code || newCode == "" {
		t.Fatalf("rotate did not mint a new code")
	}
	if rec := do(t, h, http.MethodGet, "/d/"+key+"/v/1", codeAuth, ""); rec.Code != http.StatusNotFound {
		t.Errorf("old code after rotate = %d; want 404", rec.Code)
	}
}

// TestCodeCookieExchange verifies a browser ?code= is exchanged for an HttpOnly
// cookie and redirected to a param-free URL, so the secret leaves the address bar.
func TestCodeCookieExchange(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	pb := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"doc","html":"<html><body><p>hi</p></body></html>"}`)
	key := pubKey(t, pb)
	sh := do(t, h, http.MethodPost, "/v1/docs/"+key+"/share", authorHdrNoCT(), "")
	var share map[string]any
	_ = json.Unmarshal(sh.Body.Bytes(), &share)
	code, _ := share["data"].(map[string]any)["code"].(string)

	rec := do(t, h, http.MethodGet, "/d/"+key+"/v/1?code="+code, nil, "")
	if rec.Code != http.StatusFound {
		t.Fatalf("?code= first hit = %d; want 302 redirect", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc == "" || contains(loc, "code=") {
		t.Errorf("redirect Location %q must not contain the code", loc)
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if setCookie == "" || !contains(setCookie, "HttpOnly") {
		t.Errorf("expected an HttpOnly capability cookie, got %q", setCookie)
	}
}

// TestAuthorMutationsGatedByCreator verifies author-only mutations (share, draft
// save/promote) require the creator's session (trust-header uid), and that a
// reader share-code credential never authorizes them. Write tokens are no longer
// an author credential, so the old "write-token via cookie" path is gone; a
// browser now carries author identity via the reverse-proxy trust headers.
func TestAuthorMutationsGatedByCreator(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	pb := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"br","html":"<html><body><p>hi</p></body></html>"}`)
	key := pubKey(t, pb)

	// The creator (trust-header uid == stamped creator_uid) authorizes share.
	rec := do(t, h, http.MethodPost, "/v1/docs/"+key+"/share", authorHdrNoCT(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("share as creator = %d; want 200: %s", rec.Code, rec.Body.String())
	}

	// A save-draft + promote as the creator must also work.
	rec = do(t, h, http.MethodPut, "/v1/docs/"+key+"/draft", authorHdr(),
		`{"html":"<html><body><h1>draft</h1></body></html>"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("draft save as creator = %d; want 200: %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPost, "/v1/docs/"+key+"/draft/promote", authorHdrNoCT(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("promote as creator = %d; want 200: %s", rec.Code, rec.Body.String())
	}

	// A reader code cookie must NOT authorize author mutations.
	sh := do(t, h, http.MethodPost, "/v1/docs/"+key+"/share", authorHdrNoCT(), "")
	var share map[string]any
	_ = json.Unmarshal(sh.Body.Bytes(), &share)
	readerCode, _ := share["data"].(map[string]any)["code"].(string)
	readerCookie := capCookie(key, readerCode)
	rec = do(t, h, http.MethodPost, "/v1/docs/"+key+"/draft/promote", map[string]string{"Cookie": readerCookie}, "")
	if rec.Code == http.StatusOK {
		t.Error("a reader code must not authorize promote")
	}

	// A different signed-in uid (not the creator) must NOT authorize mutations.
	rec = do(t, h, http.MethodPost, "/v1/docs/"+key+"/draft/promote",
		map[string]string{octoUIDHeaderName: "someone-else"}, "")
	if rec.Code == http.StatusOK {
		t.Error("a non-creator session must not authorize promote")
	}
}

// TestFreshCodeBeatsStaleCookie guards the credential-precedence fix: a browser
// holding an old capability cookie that is then handed a fresh valid ?code= link
// must be authorized by the code, not shadowed by the stale cookie. Covers both
// failure modes: (a) a revoked reader code cut off despite a valid new code, and
// (b) an author's ?code=<write-token> blocked by a pre-existing reader cookie.
func TestFreshCodeBeatsStaleCookie(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	pb := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"rot","html":"<html><body><p>hi</p></body></html>"}`)
	key := pubKey(t, pb)

	mintCode := func() string {
		sh := do(t, h, http.MethodPost, "/v1/docs/"+key+"/share", authorHdrNoCT(), "")
		var share map[string]any
		_ = json.Unmarshal(sh.Body.Bytes(), &share)
		code, _ := share["data"].(map[string]any)["code"].(string)
		return code
	}

	// (a) Rotate: an old code's cookie must not block a freshly rotated code link.
	oldCode := mintCode()
	newCode := mintCode() // rotation invalidates oldCode's hash
	staleCookie := capCookie(key, oldCode)
	rec := do(t, h, http.MethodGet, "/d/"+key+"/v/1?code="+newCode, map[string]string{"Cookie": staleCookie}, "")
	if rec.Code != http.StatusFound {
		t.Fatalf("fresh code with stale cookie = %d; want 302 (code honored): %s", rec.Code, rec.Body.String())
	}
	// The exchange must re-issue the cookie with the winning (new) code.
	if sc := rec.Header().Get("Set-Cookie"); !contains(sc, newCode) {
		t.Errorf("exchange should store the new code; Set-Cookie = %q", sc)
	}

	// (b) A second rotation invalidates the code held in a browser's cookie; a
	// fresh ?code= link must still be honored (code wins over the now-stale
	// cookie), re-issuing the cookie with the latest code. Write tokens are no
	// longer a doc credential, so this precedence is exercised via share codes.
	staleReaderCookie := capCookie(key, newCode)
	newestCode := mintCode() // invalidates newCode's hash
	rec = do(t, h, http.MethodGet, "/d/"+key+"/v/1?code="+newestCode, map[string]string{"Cookie": staleReaderCookie}, "")
	if rec.Code != http.StatusFound {
		t.Fatalf("fresh code with stale reader cookie = %d; want 302 (code honored): %s", rec.Code, rec.Body.String())
	}
	if sc := rec.Header().Get("Set-Cookie"); !contains(sc, newestCode) {
		t.Errorf("exchange should store the newest code; Set-Cookie = %q", sc)
	}
}

// TestRenderCapMarkerReflectsViewer asserts the render injects window.__ODOC_CAP__
// as the four-tier capability object. isAuthor follows canManage (not canEdit),
// so a writer does not receive creator/admin management UI. The creator
// resolves to manage (admin); a share-code reader gets read-only.
func TestRenderCapMarkerReflectsViewer(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	pb := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"cap","html":"<html><body><p>hi</p></body></html>"}`)
	key := pubKey(t, pb)
	sh := do(t, h, http.MethodPost, "/v1/docs/"+key+"/share", authorHdrNoCT(), "")
	var share map[string]any
	_ = json.Unmarshal(sh.Body.Bytes(), &share)
	code, _ := share["data"].(map[string]any)["code"].(string)

	// Author (creator uid via trust header) → manage tier: canManage/canEdit/
	// canComment/canRead all true, role admin, isAuthor alias true.
	rec := do(t, h, http.MethodGet, "/d/"+key+"/v/1", authorHdrNoCT(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("author render = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`role: "admin"`, `canManage: true`, `canEdit: true`, `canComment: true`, `canRead: true`, `isAuthor: true`} {
		if !contains(body, want) {
			t.Errorf("author render marker missing %q: %s", want, body)
		}
	}

	// Reader (share code cookie) → read-only: role reader, only canRead true.
	rec = do(t, h, http.MethodGet, "/d/"+key+"/v/1", map[string]string{"Cookie": capCookie(key, code)}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reader render = %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{`role: "reader"`, `canRead: true`, `canComment: false`, `canEdit: false`, `canManage: false`, `isAuthor: false`} {
		if !contains(body, want) {
			t.Errorf("reader render marker missing %q: %s", want, body)
		}
	}
}

func TestDraftRenderInjectsEditorCapabilityMarker(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPut, "/v1/docs/draft-cap/draft", authorHdr(),
		`{"html":"<html><body><p>draft</p></body></html>"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save draft = %d: %s", rec.Code, rec.Body.String())
	}
	key := pubKey(t, rec)

	rec = do(t, h, http.MethodGet, "/d/"+key+"/draft", authorHdrNoCT(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("draft render = %d: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`role: "admin"`, `canManage: true`, `canEdit: true`, `canComment: true`, `canRead: true`} {
		if !contains(rec.Body.String(), want) {
			t.Errorf("draft render marker missing %q: %s", want, rec.Body.String())
		}
	}
}

func TestCapMarkerWriterIsNotAuthorManager(t *testing.T) {
	html := httpx.InjectCapMarkerForTest("<script>window.__ODOC__ = {};</script>", service.CapEdit)
	for _, want := range []string{`role: "writer"`, `canEdit: true`, `canManage: false`, `isAuthor: false`} {
		if !contains(html, want) {
			t.Errorf("writer marker missing %q: %s", want, html)
		}
	}
}

func TestOverlayConsumesCommentCapability(t *testing.T) {
	for _, want := range []string{
		"window.__ODOC_CAP__.canComment",
		"if (!canComment) return; // read-only: no new comments",
		"canComment ? renderReactInline",
		"canComment ? `<span class=\"odoc-reply-toggle\"",
		"canComment ? `<div class=\"odoc-reply-form\"",
		"canComment ? `<div class=\"odoc-anchor-actions\"",
	} {
		if !contains(assets.OverlayJS, want) {
			t.Errorf("overlay missing read-only capability gate %q", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestFrameAncestorsCSPHeader locks the OCT-138 iframe-embed contract: every
// /d/{slug}/v/{version} render carries a CSP frame-ancestors value driven by
// FRAME_ANCESTORS. Verifies (a) the configured allowlist reaches the wire so
// octo-web's iframe embed actually works, (b) X-Frame-Options relaxes to
// SAMEORIGIN when embedding is enabled (DENY would silently defeat the CSP),
// and (c) 'none' still blocks framing (safe default for stand-alone deploys).
func TestFrameAncestorsCSPHeader(t *testing.T) {
	publish := func(h http.Handler) string {
		auth := authorHdr()
		pub := do(t, h, http.MethodPost, "/v1/docs", auth,
			`{"slug":"embed","html":"<html><body><p>hi</p></body></html>"}`)
		if pub.Code != http.StatusOK {
			t.Fatalf("setup publish = %d: %s", pub.Code, pub.Body.String())
		}
		return pubKey(t, pub)
	}

	// (a) FRAME_ANCESTORS listing octo-web origins reaches the wire, and XFO
	// downgrades to SAMEORIGIN — DENY here would defeat the CSP silently.
	cfg := &config.Config{
		WriteToken: "test-token", MaxHTMLBytes: 1 << 20, RepoURL: "https://x",
		FrameAncestors: "'self' http://localhost:3000 https://web.octo.example.com",
	}
	h := newTestServer(t, cfg)
	key := publish(h)
	rec := do(t, h, http.MethodGet, "/d/"+key+"/v/1", authorHdrNoCT(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("render = %d: %s", rec.Code, rec.Body.String())
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !contains(csp, "frame-ancestors 'self' http://localhost:3000 https://web.octo.example.com") {
		t.Errorf("CSP missing configured frame-ancestors; got %q", csp)
	}
	if xfo := rec.Header().Get("X-Frame-Options"); xfo != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options with embed allowlist = %q; want SAMEORIGIN", xfo)
	}

	// (b) 'none' (the Load() default) keeps framing blocked — stand-alone
	// deploys stay safe unless operator opts in.
	cfg = &config.Config{
		WriteToken: "test-token", MaxHTMLBytes: 1 << 20, RepoURL: "https://x",
		FrameAncestors: "'none'",
	}
	h = newTestServer(t, cfg)
	key = publish(h)
	rec = do(t, h, http.MethodGet, "/d/"+key+"/v/1", authorHdrNoCT(), "")
	if !contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("CSP should carry frame-ancestors 'none' by default; got %q", rec.Header().Get("Content-Security-Policy"))
	}
	if xfo := rec.Header().Get("X-Frame-Options"); xfo != "DENY" {
		t.Errorf("X-Frame-Options with 'none' = %q; want DENY", xfo)
	}
}

// TestRateLimitIgnoresSpoofedXFF verifies that, without TrustProxyHeaders, a
// client cannot mint a fresh rate-limit bucket by rotating X-Forwarded-For — the
// socket peer (shared in httptest) is used, so the shared limit still applies.
func TestRateLimitIgnoresSpoofedXFF(t *testing.T) {
	cfg := &config.Config{
		WriteToken: "test-token", MaxHTMLBytes: 1 << 20, RepoURL: "https://x",
		RateLimitWindow:   60_000_000_000, // 1m in ns
		RateLimitMax:      2,
		TrustProxyHeaders: false,
	}
	h := newTestServer(t, cfg)

	// Publish slug "d" so the creator has author cap; reactions are
	// capability-gated, so without a real doc + credential they would 404 at the
	// capability check before reaching the rate limiter.
	pb := do(t, h, http.MethodPost, "/v1/docs", authorHdr(),
		`{"slug":"d","version":1,"html":"<html><body><p>x</p></body></html>"}`)
	if pb.Code != http.StatusOK {
		t.Fatalf("setup publish = %d: %s", pb.Code, pb.Body.String())
	}
	key := pubKey(t, pb)

	// Reactions are capability-gated now; use the creator's trust-header session
	// (author) so the request reaches the rate limiter rather than 404ing at the
	// capability check.
	base := authorHdr()

	got429 := false
	for i := 0; i < 6; i++ {
		// Each request spoofs a distinct XFF; it must be ignored.
		hdr := map[string]string{"X-Forwarded-For": randIP(i)}
		for k, v := range base {
			hdr[k] = v
		}
		rec := do(t, h, http.MethodPost, "/v1/reactions", hdr, `{"slug":"`+key+`","comment_id":"c","emoji":"x"}`)
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Error("spoofed X-Forwarded-For evaded the rate limit (headers trusted when they should not be)")
	}
}

func randIP(i int) string {
	return "10.0.0." + string(rune('1'+i))
}

// TestHealthzReportsUnhealthy verifies /healthz returns 503 when a store health
// check fails.
func TestHealthzReportsUnhealthy(t *testing.T) {
	h := newTestServerWithHealth(t, func() error { return errUnhealthy })
	rec := do(t, h, http.MethodGet, "/healthz", nil, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unhealthy /healthz = %d; want 503", rec.Code)
	}

	ok := newTestServerWithHealth(t, func() error { return nil })
	rec = do(t, ok, http.MethodGet, "/healthz", nil, "")
	if rec.Code != http.StatusOK {
		t.Errorf("healthy /healthz = %d; want 200", rec.Code)
	}
}
