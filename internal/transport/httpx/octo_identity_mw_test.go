package httpx_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/lml2468/octo-doc/internal/config"
	"github.com/lml2468/octo-doc/internal/platform/log"
	"github.com/lml2468/octo-doc/internal/platform/sluglock"
	"github.com/lml2468/octo-doc/internal/service"
	"github.com/lml2468/octo-doc/internal/service/octoidentity"
	"github.com/lml2468/octo-doc/internal/storage"
	"github.com/lml2468/octo-doc/internal/storage/memory"
	"github.com/lml2468/octo-doc/internal/transport/httpx"
)

// OCT-145 方案 C identity integration tests. Every request arrives from an
// internal reverse proxy (octo-server docs_proxy) that has already
// authenticated the caller and forwarded X-Octo-Uid/Name/Role. doc trusts the
// headers verbatim (it only listens on the internal network). No userinfo
// round trip; no OAuth two-hop; the middleware is header-in, session-in-context-out.

// newTestServerWithProxyIdentity wires an in-memory server with the trust-header
// identity middleware. The middleware is always on (no wiring flag) — a request
// without the headers is anonymous, same as before.
func newTestServerWithProxyIdentity(t *testing.T) http.Handler {
	t.Helper()
	cfg := &config.Config{
		WriteToken:     "test-token",
		MaxHTMLBytes:   5 << 20,
		MaxAssetBytes:  25 << 20,
		RepoURL:        "https://example.com/repo",
		AssetMIMEAllow: []string{"image/png"},
		LoginEnabled:   true,
	}
	store := memory.New()
	locker := sluglock.NewMemory()
	comments := service.NewCommentService(store, locker)
	docs := service.NewDocService(store, store, comments, locker, cfg.BaseURL, cfg.MaxHTMLBytes)
	assets := service.NewAssetService(store, store, locker, cfg.MaxAssetBytes, cfg.AssetMIMEAllow)
	auth := service.NewAuthService(store, cfg, locker)
	srv := httpx.New(httpx.Deps{
		Config: cfg, Logger: log.New("silent"), Docs: docs, Comments: comments,
		Assets: assets, Auth: auth,
		OverlayJS: "/* overlay */",
	})
	return srv.Handler()
}

// publish is a helper: create a doc as the test creator (trust-header uid) so
// tests can then exercise the read/comment paths under different credentials.
// The creator uid (testUID) differs from the viewer uids the tests use, so
// author access is only ever granted via superAdmin role or a share code, never
// by accidentally matching the creator.
func publish(t *testing.T, h http.Handler, slug string) {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/v1/docs",
		authorHdr(),
		`{"slug":"`+slug+`","version":1,"html":"<html><body><p>hello</p></body></html>","meta":{"title":"T"}}`)
	if rec.Code != 200 {
		t.Fatalf("publish %s = %d: %s", slug, rec.Code, rec.Body.String())
	}
}

// generateShareCode calls the author-only share endpoint (as the creator) and
// returns the code.
func generateShareCode(t *testing.T, h http.Handler, slug string) string {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/v1/docs/"+slug+"/share",
		authorHdrNoCT(), "")
	if rec.Code != 200 {
		t.Fatalf("share = %d: %s", rec.Code, rec.Body.String())
	}
	var env map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	data, _ := env["data"].(map[string]any)
	code, _ := data["code"].(string)
	if code == "" {
		t.Fatalf("share body missing code: %s", rec.Body.String())
	}
	return code
}

// proxyHeaders is a shorthand for the three trust headers a proxied request
// carries. The values here are what octo-server's docs_proxy would forward
// after its own auth pass.
func proxyHeaders(uid, name, role string) map[string]string {
	return map[string]string{
		"X-Octo-Uid":  uid,
		"X-Octo-Name": name,
		"X-Octo-Role": role,
	}
}

// with copies base and adds extras; used to compose headers per-request.
func with(base map[string]string, extras ...map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for _, m := range extras {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// §C.1: X-Octo-Role=superAdmin → CapAuthor grant. Read + comment succeed
// without any share code or bearer, and the octo uid is the stamped author.
func TestProxySuperAdminGrantsAuthor(t *testing.T) {
	h := newTestServerWithProxyIdentity(t)
	publish(t, h, "docA")

	rec := do(t, h, http.MethodGet, "/d/docA/v/1",
		proxyHeaders("u-admin", "Admin", "superAdmin"), "")
	if rec.Code != 200 {
		t.Fatalf("superAdmin render = %d; want 200 (CapAuthor)", rec.Code)
	}

	rec = do(t, h, http.MethodPost, "/v1/comments",
		with(proxyHeaders("u-admin", "Admin", "superAdmin"),
			map[string]string{"Content-Type": "application/json"}),
		`{"slug":"docA","text":"admin note","version":1,"anchor":{"kind":"text","text":"hello"}}`)
	if rec.Code != 200 {
		t.Fatalf("superAdmin comment = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"u-admin"`) {
		t.Errorf("expected octo uid in comment response: %s", rec.Body.String())
	}
}

// §C.2: non-superAdmin roles ("admin", "member") do NOT grant CapAuthor. Only
// role=="superAdmin" upgrades; everything else needs a share code / write token
// / doc_binding to see the doc.
func TestProxyAdminAndMemberDoNotGrantAuthor(t *testing.T) {
	h := newTestServerWithProxyIdentity(t)
	publish(t, h, "docB")

	for _, role := range []string{"admin", "member"} {
		rec := do(t, h, http.MethodGet, "/d/docB/v/1",
			proxyHeaders("u-1", "User", role), "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("role=%q render = %d; want 404 (not superAdmin, no share code)", role, rec.Code)
		}
	}
}

// §C.3: non-superAdmin identity + share code → CapReader (share code path),
// and the comment author uid comes from the trust headers, not the code.
func TestProxyIdentityPlusShareCodeGrantsReader(t *testing.T) {
	h := newTestServerWithProxyIdentity(t)
	publish(t, h, "docC")
	code := generateShareCode(t, h, "docC")

	rec := do(t, h, http.MethodPost, "/v1/comments",
		with(proxyHeaders("u-42", "User", "member"),
			map[string]string{"Authorization": "Bearer " + code, "Content-Type": "application/json"}),
		`{"slug":"docC","text":"hi","version":1,"anchor":{"kind":"text","text":"hello"}}`)
	if rec.Code != 200 {
		t.Fatalf("reader+identity comment = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"u-42"`) {
		t.Errorf("expected octo uid u-42 in comment author: %s", rec.Body.String())
	}
}

// §C.4: no identity headers, no share code → 404 (hidden existence).
func TestProxyNoIdentityNoCred404(t *testing.T) {
	h := newTestServerWithProxyIdentity(t)
	publish(t, h, "docD")

	rec := do(t, h, http.MethodGet, "/d/docD/v/1", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("anonymous render = %d; want 404", rec.Code)
	}
}

// §C.5: empty X-Octo-Uid → no session mounted, even if Name/Role are set. The
// middleware treats empty uid as "no octo identity"; without one, role alone
// cannot grant CapAuthor.
func TestProxyEmptyUIDNoIdentity(t *testing.T) {
	h := newTestServerWithProxyIdentity(t)
	publish(t, h, "docE")

	rec := do(t, h, http.MethodGet, "/d/docE/v/1",
		proxyHeaders("", "Admin", "superAdmin"), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty-uid render = %d; want 404 (no identity → no superAdmin grant)", rec.Code)
	}
}

// §C.6: session grant is context-only. A CapAuthor grant from superAdmin must
// NOT write a per-doc cookie into the response — the invariant is that only
// raw share codes cookieise, session grants never do.
func TestProxySuperAdminDoesNotSetCookie(t *testing.T) {
	h := newTestServerWithProxyIdentity(t)
	publish(t, h, "docF")

	rec := do(t, h, http.MethodGet, "/d/docF/v/1",
		proxyHeaders("u-admin", "Admin", "superAdmin"), "")
	if rec.Code != 200 {
		t.Fatalf("superAdmin render = %d", rec.Code)
	}
	if sc := rec.Header().Get("Set-Cookie"); sc != "" {
		t.Fatalf("superAdmin grant must not set a cookie; got Set-Cookie: %q", sc)
	}
}

// §C.7: /v1/auth/me surfaces the proxy-derived identity so the overlay can
// render the current user without any additional round trip.
func TestAuthMeReflectsProxyIdentity(t *testing.T) {
	h := newTestServerWithProxyIdentity(t)

	rec := do(t, h, http.MethodGet, "/v1/auth/me",
		proxyHeaders("u-42", "Alice", "member"), "")
	if rec.Code != 200 {
		t.Fatalf("auth/me = %d: %s", rec.Code, rec.Body.String())
	}
	var env map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	data, _ := env["data"].(map[string]any)
	id, _ := data["identity"].(map[string]any)
	if id["login"] != "u-42" || id["name"] != "Alice" {
		t.Fatalf("identity = %+v", id)
	}
	if data["isOwner"] != false {
		t.Fatalf("member must not be owner: %+v", data)
	}
	if data["authConfigured"] != true {
		t.Fatalf("authConfigured must be true in fusion: %+v", data)
	}
}

// §C.8: session takes precedence over the legacy cookie session. A stale
// odoc_sid cookie must never demote a currently-signed-in octo user.
func TestProxyIdentityBeatsLegacyCookieSession(t *testing.T) {
	// Manually seed a legacy cookie session in storage so the resolver can find it.
	store := memory.New()
	locker := sluglock.NewMemory()
	sid := "legacy-sid"
	_ = store.PutSession(context.Background(), sid, storage.Session{Login: "legacy", Name: "Legacy", Created: "t"}, 3600)
	cfg := &config.Config{
		WriteToken:     "test-token",
		MaxHTMLBytes:   5 << 20,
		MaxAssetBytes:  25 << 20,
		AssetMIMEAllow: []string{"image/png"},
	}
	comments := service.NewCommentService(store, locker)
	docs := service.NewDocService(store, store, comments, locker, cfg.BaseURL, cfg.MaxHTMLBytes)
	assets := service.NewAssetService(store, store, locker, cfg.MaxAssetBytes, cfg.AssetMIMEAllow)
	auth := service.NewAuthService(store, cfg, locker)
	srv := httpx.New(httpx.Deps{
		Config: cfg, Logger: log.New("silent"), Docs: docs, Comments: comments,
		Assets: assets, Auth: auth, OverlayJS: "/* overlay */",
	})
	h := srv.Handler()

	rec := do(t, h, http.MethodGet, "/v1/auth/me",
		with(proxyHeaders("u-42", "Alice", "member"),
			map[string]string{"Cookie": "odoc_sid=" + sid}), "")
	if rec.Code != 200 {
		t.Fatalf("auth/me = %d", rec.Code)
	}
	var env map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	data, _ := env["data"].(map[string]any)
	id, _ := data["identity"].(map[string]any)
	if id["login"] != "u-42" {
		t.Fatalf("expected octo identity to win over legacy cookie; got %+v", id)
	}
}

// §C.9 regression for OCT-133 URL-cleaning: when a superAdmin opens a doc URL
// with ?code=<share>, the code must be stripped from the URL (302 + HttpOnly
// cookie) even though the session — not the code — is what actually grants
// CapAuthor. Otherwise the reader share code lingers in address bar/history/
// Referer. The cookie stores the raw share code; the session grant itself
// never lands in a cookie.
func TestProxySuperAdminStripsShareCodeFromURL(t *testing.T) {
	h := newTestServerWithProxyIdentity(t)
	publish(t, h, "docG")
	code := generateShareCode(t, h, "docG")

	rec := do(t, h, http.MethodGet, "/d/docG/v/1?code="+code,
		proxyHeaders("u-admin", "Admin", "superAdmin"), "")
	if rec.Code != http.StatusFound {
		t.Fatalf("admin + ?code render = %d; want 302 (code must not linger in URL)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "code=") {
		t.Fatalf("redirect Location still carries code: %q", loc)
	}
	setCookie := rec.Header().Get("Set-Cookie")
	wantName := capCookieNameForTest("docG")
	if !strings.Contains(setCookie, wantName+"=") {
		t.Fatalf("Set-Cookie missing share code under %s: %q", wantName, setCookie)
	}
}

func TestBotAuthMiddlewareMountsBotSessionNotSuperAdmin(t *testing.T) {
	// A verified bot session carries the bot's own uid as login but is NOT a
	// global superAdmin (author now comes from OwnerUID matching creator_uid, not
	// a blanket role). Guards against the retired "every bot is superAdmin" grant.
	withStubIdentity(t, stubIdentity{botUID: "botuid", botName: "Deploy Bot", botSpaceID: "space1", botOwnerUID: "owner-u"})
	h := newTestServer(t, &config.Config{
		WriteToken:        "test-token",
		MaxHTMLBytes:      5 << 20,
		MaxAssetBytes:     25 << 20,
		AssetMIMEAllow:    []string{"image/png"},
		OctoServerBaseURL: "http://octo.example",
		BotAuthEnabled:    true,
	})

	rec := do(t, h, http.MethodGet, "/v1/auth/me", map[string]string{"Authorization": "Bearer bot-token"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("auth/me = %d: %s", rec.Code, rec.Body.String())
	}
	var env map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	data, _ := env["data"].(map[string]any)
	id, _ := data["identity"].(map[string]any)
	if id["login"] != "botuid" || id["name"] != "Deploy Bot" {
		t.Fatalf("bot identity = %+v", id)
	}
	if data["isOwner"] != false {
		t.Fatalf("bot session must NOT be superAdmin owner: %+v", data)
	}
}

func TestBotAuthMiddlewareDisabledDoesNotMountSession(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "botuid", botName: "Deploy Bot", botSpaceID: "space1"})
	h := newTestServer(t, &config.Config{
		WriteToken:        "test-token",
		MaxHTMLBytes:      5 << 20,
		MaxAssetBytes:     25 << 20,
		AssetMIMEAllow:    []string{"image/png"},
		OctoServerBaseURL: "http://octo.example",
		BotAuthEnabled:    false,
	})

	rec := do(t, h, http.MethodGet, "/v1/auth/me", map[string]string{"Authorization": "Bearer bot-token"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("auth/me = %d: %s", rec.Code, rec.Body.String())
	}
	var env map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	data, _ := env["data"].(map[string]any)
	if data["identity"] != nil {
		t.Fatalf("bot auth disabled must not mount identity: %+v", data["identity"])
	}
}

func TestPublishAcceptsBotOwnerOrTrustHeaderSession(t *testing.T) {
	botCfg := func(enabled bool) *config.Config {
		return &config.Config{
			WriteToken:        "test-token",
			MaxHTMLBytes:      5 << 20,
			MaxAssetBytes:     25 << 20,
			AssetMIMEAllow:    []string{"image/png"},
			OctoServerBaseURL: "http://octo.example",
			BotAuthEnabled:    enabled,
		}
	}
	body := func(slug string) string {
		return `{"slug":"` + slug + `","version":1,"html":"<html><body><p>hello</p></body></html>","meta":{"title":"T"}}`
	}

	t.Run("write token without provider rejected", func(t *testing.T) {
		// Write tokens are no longer an auth source for publish. With no identity
		// provider wired, a bare Bearer test-token builds no session (neither bot
		// nor user verify runs), so requireWriteOrBotOwnerAuth rejects it. (When a
		// bot provider is present and accepts the token, publish is authorized via
		// the bot session, not the write token — see the bot-owner subtest.)
		octoidentity.Set(nil)
		t.Cleanup(func() { octoidentity.Set(nil) })
		h := newTestServer(t, botCfg(true))
		rec := do(t, h, http.MethodPost, "/v1/docs",
			map[string]string{"Authorization": "Bearer test-token", "Content-Type": "application/json"},
			body("publishWriteToken"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("publish with write token = %d; want 401 (write token retired)", rec.Code)
		}
	})

	t.Run("bot owner session", func(t *testing.T) {
		withStubIdentity(t, stubIdentity{botUID: "botuid", botName: "Deploy Bot", botSpaceID: "space1"})
		h := newTestServer(t, botCfg(true))
		rec := do(t, h, http.MethodPost, "/v1/docs",
			map[string]string{"Authorization": "Bearer bot-token", "Content-Type": "application/json"},
			body("publishBotOwner"))
		if rec.Code != http.StatusOK {
			t.Fatalf("publish with bot owner session = %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("trust header session", func(t *testing.T) {
		// A proxy-forwarded trust-header identity builds a session (Login set), so
		// requireWriteOrBotOwnerAuth accepts it and stamps that uid as creator. The
		// doc listens only on the internal network, so the headers are trusted.
		withStubIdentity(t, stubIdentity{botUID: "botuid", botName: "Deploy Bot", botSpaceID: "space1"})
		h := newTestServer(t, botCfg(true))
		rec := do(t, h, http.MethodPost, "/v1/docs",
			map[string]string{
				"Content-Type": "application/json",
				"X-Octo-Uid":   "trusted-user",
				"X-Octo-Role":  "member",
			},
			body("publishTrustHeaderSession"))
		if rec.Code != http.StatusOK {
			t.Fatalf("publish with trust-header session = %d; want 200", rec.Code)
		}
	})

	t.Run("no credential", func(t *testing.T) {
		withStubIdentity(t, stubIdentity{botUID: "botuid", botName: "Deploy Bot", botSpaceID: "space1"})
		h := newTestServer(t, botCfg(true))
		rec := do(t, h, http.MethodPost, "/v1/docs",
			map[string]string{"Content-Type": "application/json"},
			body("publishNoCredential"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("publish without credential = %d; want 401", rec.Code)
		}
	})

	t.Run("bot auth disabled", func(t *testing.T) {
		withStubIdentity(t, stubIdentity{botUID: "botuid", botName: "Deploy Bot", botSpaceID: "space1"})
		h := newTestServer(t, botCfg(false))
		rec := do(t, h, http.MethodPost, "/v1/docs",
			map[string]string{"Authorization": "Bearer bot-token", "Content-Type": "application/json"},
			body("publishBotDisabled"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("publish with bot auth disabled = %d; want 401", rec.Code)
		}
	})
}

func TestBotAuthEnabledProviderDisabledWriteTokenNoLongerAuthorizes(t *testing.T) {
	octoidentity.Set(nil)
	t.Cleanup(func() { octoidentity.Set(nil) })
	h := newTestServer(t, &config.Config{
		WriteToken:        "test-token",
		MaxHTMLBytes:      5 << 20,
		MaxAssetBytes:     25 << 20,
		AssetMIMEAllow:    []string{"image/png"},
		OctoServerBaseURL: "http://octo.example",
		BotAuthEnabled:    true,
	})
	publish(t, h, "botFallbackWrite")

	// Write tokens no longer grant author. With the bot provider disabled there is
	// no session behind Bearer test-token, so the author-only delete is hidden
	// (404) rather than authorized.
	rec := do(t, h, http.MethodDelete, "/v1/docs/botFallbackWrite", map[string]string{"Authorization": "Bearer test-token"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete with write token (retired) = %d; want 404: %s", rec.Code, rec.Body.String())
	}

	// The creator (trust-header uid == stamped creator_uid) still authorizes it.
	rec = do(t, h, http.MethodDelete, "/v1/docs/botFallbackWrite", authorHdrNoCT(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete as creator = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBotAuthNilIdentityAllowsShareCodeReader(t *testing.T) {
	withStubIdentity(t, stubIdentity{})
	h := newTestServer(t, &config.Config{
		WriteToken:        "test-token",
		MaxHTMLBytes:      5 << 20,
		MaxAssetBytes:     25 << 20,
		AssetMIMEAllow:    []string{"image/png"},
		OctoServerBaseURL: "http://octo.example",
		BotAuthEnabled:    true,
	})
	publish(t, h, "botFallbackReader")
	code := generateShareCode(t, h, "botFallbackReader")

	rec := do(t, h, http.MethodGet, "/v1/docs/botFallbackReader/versions", map[string]string{"Authorization": "Bearer " + code}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("versions with share code and nil bot identity = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBotAuthMiddlewareDoesNotOverrideProxySession(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "botuid", botName: "Deploy Bot", botSpaceID: "space1"})
	h := newTestServer(t, &config.Config{
		WriteToken:        "test-token",
		MaxHTMLBytes:      5 << 20,
		MaxAssetBytes:     25 << 20,
		AssetMIMEAllow:    []string{"image/png"},
		OctoServerBaseURL: "http://octo.example",
		BotAuthEnabled:    true,
	})

	rec := do(t, h, http.MethodGet, "/v1/auth/me",
		with(proxyHeaders("u-trust", "Trust User", "member"), map[string]string{"Authorization": "Bearer bot-token"}), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("auth/me = %d: %s", rec.Code, rec.Body.String())
	}
	var env map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	data, _ := env["data"].(map[string]any)
	id, _ := data["identity"].(map[string]any)
	if id["login"] != "u-trust" || id["name"] != "Trust User" {
		t.Fatalf("proxy session should win over bot identity: %+v", id)
	}
}

// capCookieNameForTest mirrors httpx.capCookieName (unexported) via storage.HashSlug
// so the test can set the reader cookie directly without simulating a full
// ?code=→cookie exchange.
func capCookieNameForTest(slug string) string { return "octo_cap_" + storage.HashSlug(slug) }
