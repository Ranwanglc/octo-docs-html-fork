package httpx_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/assets"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/log"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/transport/httpx"
)

// newTestServer builds a full server backed by the in-memory store.
func newTestServer(t *testing.T, cfg *config.Config) http.Handler {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{
			WriteToken: "test-token", MaxHTMLBytes: 5 << 20, RepoURL: "https://example.com/repo",
			RateLimitMax:   0, // disable rate limiting in tests
			MaxAssetBytes:  25 << 20,
			AssetMIMEAllow: []string{"image/png", "image/gif", "image/jpeg"},
		}
	}
	store := memory.New()
	locker := sluglock.NewMemory()
	comments := service.NewCommentService(store, locker)
	docs := service.NewDocService(store, store, comments, locker, cfg.BaseURL, cfg.MaxHTMLBytes)
	assets := service.NewAssetService(store, store, locker, cfg.MaxAssetBytes, cfg.AssetMIMEAllow)
	auth := service.NewAuthService(store, cfg, locker)
	srv := httpx.New(httpx.Deps{
		Config: cfg, Logger: log.New("silent"), Docs: docs, Comments: comments, Assets: assets, Auth: auth,
		OverlayJS: "/* overlay */",
	})
	return srv.Handler()
}

// testUID is the octo uid used by tests to seed publishes and drive author-only
// operations. Under the creator-auth model, publishing with X-Octo-Uid:<testUID>
// stamps this uid as the doc's creator_uid, so subsequent author ops sent with
// the same trust-header uid resolve to CapAuthor. This replaced the retired
// write-token ("Bearer test-token") as the seed/author credential.
const testUID = "test-uid"

// authorHdr returns the trust-header identity map an author uses for JSON writes
// (publish/draft/promote/comment). octoIdentityMiddleware trusts X-Octo-* as the
// reverse proxy would forward them.
func authorHdr() map[string]string {
	return map[string]string{octoUIDHeaderName: testUID, "Content-Type": "application/json"}
}

// authorHdrNoCT is authorHdr without a Content-Type, for author reads (GET
// versions, render draft/version) that carry no body.
func authorHdrNoCT() map[string]string {
	return map[string]string{octoUIDHeaderName: testUID}
}

// octoUIDHeaderName mirrors the unexported octoUIDHeader constant so external
// (_test package) fixtures can set the trust header without importing internals.
const octoUIDHeaderName = "X-Octo-Uid"
const octoRoleHeaderName = "X-Octo-Role"

// adminHdr / adminHdrNoCT are superAdmin trust headers. A superAdmin has author
// capability on any slug (IsOwner short-circuit in bestCred), including a doc
// that does not exist yet — the only identity that can create a doc via the
// draft-first path (SaveDraft), since draft save does not stamp a creator_uid
// the way publish does.
func adminHdr() map[string]string {
	return map[string]string{octoUIDHeaderName: "admin-uid", octoRoleHeaderName: "superAdmin", "Content-Type": "application/json"}
}

func adminHdrNoCT() map[string]string {
	return map[string]string{octoUIDHeaderName: "admin-uid", octoRoleHeaderName: "superAdmin"}
}

func do(t *testing.T, h http.Handler, method, target string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, target, r)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// pubKey extracts the server-derived storage key (data.slug) from a publish
// response envelope. Under the doc-key identity model a publish with a creator
// identity stores the doc under a derived key, not the client-sent slug, so
// post-publish addressing (render/versions/share/comments/assets/delete) must
// use this returned key rather than the original alias.
func pubKey(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Data struct {
			Slug  string `json:"slug"`
			Alias string `json:"alias"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("pubKey: decode publish response: %v (%s)", err, rec.Body.String())
	}
	if env.Data.Slug == "" {
		t.Fatalf("pubKey: publish response carried no slug: %s", rec.Body.String())
	}
	return env.Data.Slug
}

// mustPublish publishes html under the given alias with the author identity and
// returns the derived storage key. Fails the test on a non-200.
func mustPublish(t *testing.T, h http.Handler, alias, html string) string {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(),
		`{"slug":"`+alias+`","html":`+strconv.Quote(html)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("mustPublish %q = %d: %s", alias, rec.Code, rec.Body.String())
	}
	return pubKey(t, rec)
}

// mustPublishMeta publishes a full JSON body (already-encoded) with the author
// identity and returns the derived storage key.
func mustPublishMeta(t *testing.T, h http.Handler, body string) string {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("mustPublishMeta = %d: %s", rec.Code, rec.Body.String())
	}
	return pubKey(t, rec)
}

func TestPingIdentity(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/v1/ping", nil, "")
	if rec.Code != 200 {
		t.Fatalf("ping status = %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	data, _ := body["data"].(map[string]any)
	if data == nil || data["service"] != "octo-doc" {
		t.Fatalf("ping data = %v; want data.service=octo-doc", body)
	}
}

func TestPublishRequiresAuth(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPost, "/v1/docs", map[string]string{"Content-Type": "application/json"},
		`{"slug":"x","html":"<html></html>"}`)
	if rec.Code != 401 {
		t.Fatalf("unauthenticated publish = %d; want 401", rec.Code)
	}
}

func TestPublishResponseIncludesRegistrationState(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(),
		`{"slug":"contract","html":"<html><body>x</body></html>"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data["registered"] != false || envelope.Data["status"] != "published_unregistered" {
		t.Fatalf("publish data = %#v", envelope.Data)
	}
	if envelope.Data["doc_id"] != "" || envelope.Data["share_url"] != "" {
		t.Fatalf("unregistered identifiers must be empty: %#v", envelope.Data)
	}
	if envelope.Data["url"] != "" {
		t.Fatalf("unregistered response must not expose a url: %#v", envelope.Data)
	}
	if _, ok := envelope.Data["render_url"]; ok {
		t.Fatalf("render_url must not be present in the publish response: %#v", envelope.Data)
	}
}

func TestPublishMountedFailsClosedWithoutRegistrar(t *testing.T) {
	for _, mountType := range []string{"group", "thread"} {
		t.Run(mountType, func(t *testing.T) {
			h := newTestServer(t, nil)
			body := fmt.Sprintf(`{"slug":"contract-%s","html":"<html><body>x</body></html>","mount_type":%q}`, mountType, mountType)
			rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), body)
			if rec.Code != http.StatusOK {
				t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
			}
			var envelope struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Data["registered"] != false || envelope.Data["status"] != "registration_failed" {
				t.Fatalf("publish data = %#v", envelope.Data)
			}
		})
	}
}

func TestPublishOmittedOrEmptyMountPreservesExistingMount(t *testing.T) {
	h := newTestServer(t, nil)
	for _, body := range []string{
		`{"slug":"mounted-presence","html":"<html><body>v1</body></html>","mount_type":"group"}`,
		`{"slug":"mounted-presence","html":"<html><body>v2</body></html>"}`,
		`{"slug":"mounted-presence","html":"<html><body>v3</body></html>","mount_type":""}`,
	} {
		rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), body)
		if rec.Code != http.StatusOK {
			t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
		}
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Data["status"] != "registration_failed" {
			t.Fatalf("publish data = %#v; mount context was not preserved", envelope.Data)
		}
	}
}

func TestPublishRejectsInvalidMountType(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(),
		`{"slug":"bad-mount","html":"<html><body>x</body></html>","mount_type":"gruop"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "VALIDATION_ERROR" || envelope.Error.Details["code"] != "mount_type_invalid" {
		t.Fatalf("error = %#v; want VALIDATION_ERROR/mount_type_invalid", envelope.Error)
	}
	rec = do(t, h, http.MethodGet, "/v1/docs/bad-mount/versions", authorHdrNoCT(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("invalid publish persisted doc: versions = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPublishTitleFromMeta(t *testing.T) {
	// The CLI sends the doc's meta.json under `meta` ({slug,version,html,meta,
	// comments}); the server must read meta.title when no top-level title is given.
	h := newTestServer(t, nil)
	auth := authorHdr()
	rec := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"titled","version":1,"html":"<html><body><h1>x</h1></body></html>","meta":{"title":"From Meta","slug":"titled"}}`)
	if rec.Code != 200 {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	key := pubKey(t, rec)
	rec = do(t, h, http.MethodGet, "/v1/docs/"+key+"/versions", authorHdrNoCT(), "")
	if !strings.Contains(rec.Body.String(), `"title":"From Meta"`) {
		t.Fatalf("title from meta not applied: %s", rec.Body.String())
	}
}

func TestRenderAlwaysPublishedMode(t *testing.T) {
	// A doc served by this server is published — the overlay must run in
	// "published" mode (Share/Fork), never "local" (which would show a dead
	// Publish button). authConfigured is config-driven (LoginEnabled): the
	// default test cfg leaves it off (stand-alone deploy), so the overlay
	// stays anonymous.
	h := newTestServer(t, nil)
	auth := authorHdr()
	key := mustPublishMeta(t, h, `{"slug":"m","version":1,"html":"<html><body><h1>x</h1></body></html>","meta":{"title":"M"}}`)
	_ = auth
	body := do(t, h, http.MethodGet, "/d/"+key+"/v/1", authorHdrNoCT(), "").Body.String()
	if !strings.Contains(body, `"mode":"published"`) {
		t.Errorf("expected published mode in: %s", body[strings.Index(body, "__ODOC__"):min(strings.Index(body, "__ODOC__")+120, len(body))])
	}
	if !strings.Contains(body, `"authConfigured":false`) {
		t.Error("expected authConfigured=false (LoginEnabled off in default test cfg)")
	}
	// The render handler must seed __ODOC__ with the human title (data.Title from
	// meta), so the overlay top bar shows it instead of the slug.
	if !strings.Contains(body, `"title":"M"`) {
		t.Errorf("expected human title in __ODOC__: %s", body[strings.Index(body, "__ODOC__"):min(strings.Index(body, "__ODOC__")+160, len(body))])
	}
	// Field presence alone is a false-green (the value must actually be consumed).
	// Assert the real overlay source reads cfg.title so the toolbar renders the meta
	// title, not just carries it in the JSON blob. (This test injects a mock overlay
	// string, so assert against the embedded assets.OverlayJS truth source.)
	if !strings.Contains(assets.OverlayJS, "cfg.title") {
		t.Error("overlay source must consume cfg.title (toolbar should prefer meta title over <title>)")
	}
}

func TestCommentRequiresCapability(t *testing.T) {
	// Default-private: a comment with no credential is rejected (404, existence
	// hidden). Comment capability (author or commenter member) is required.
	h := newTestServer(t, nil)
	auth := authorHdr()
	key := mustPublishMeta(t, h, `{"slug":"anon","version":1,"html":"<html><body><p>hello world</p></body></html>"}`)

	// No credential → rejected.
	rec := do(t, h, http.MethodPost, "/v1/comments", map[string]string{"Content-Type": "application/json"},
		`{"slug":"`+key+`","text":"nice","version":1,"anchor":{"kind":"text","text":"hello"}}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("anonymous comment = %d; want 404 (needs a capability)", rec.Code)
	}

	// The author (write token) can comment.
	rec = do(t, h, http.MethodPost, "/v1/comments", auth,
		`{"slug":"`+key+`","text":"nice","version":1,"anchor":{"kind":"text","text":"hello"}}`)
	if rec.Code != 200 {
		t.Fatalf("author comment = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCommentMutationsHideVersionsBeforeAuthorization(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	var key string
	for _, html := range []string{
		`{"slug":"private-mutations","html":"<html><body><p>v1</p></body></html>"}`,
		`{"slug":"private-mutations","html":"<html><body><p>v2</p></body></html>"}`,
	} {
		rec := do(t, h, http.MethodPost, "/v1/docs", auth, html)
		if rec.Code != http.StatusOK {
			t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
		}
		key = pubKey(t, rec)
	}

	rec := do(t, h, http.MethodPost, "/v1/comments", auth,
		`{"slug":"`+key+`","text":"seed","version":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed comment = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Data.ID == "" {
		t.Fatalf("seed comment response = %s: %v", rec.Body.String(), err)
	}

	for _, tc := range []struct {
		name, method, target, payload string
	}{
		{"patch in range", http.MethodPatch, "/v1/comments", `{"slug":"` + key + `","id":"` + created.Data.ID + `","anchor":{"kind":"element","aid":"a"},"version":2}`},
		{"patch out of range", http.MethodPatch, "/v1/comments", `{"slug":"` + key + `","id":"` + created.Data.ID + `","anchor":{"kind":"element","aid":"a"},"version":999999}`},
		{"react in range", http.MethodPost, "/v1/reactions", `{"slug":"` + key + `","comment_id":"` + created.Data.ID + `","emoji":"x","version":2}`},
		{"react out of range", http.MethodPost, "/v1/reactions", `{"slug":"` + key + `","comment_id":"` + created.Data.ID + `","emoji":"x","version":999999}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, tc.method, tc.target, map[string]string{"Content-Type": "application/json"}, tc.payload)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPublishRenderLifecycle(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()

	// Publish v1.
	rec := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"hello","html":"<html><body><h1>Hi</h1><img src=\"a.png\"></body></html>","title":"Hello"}`)
	if rec.Code != 200 {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	var pub map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &pub)
	pubData, _ := pub["data"].(map[string]any)
	if pubData == nil || pubData["version"].(float64) != 1 {
		t.Fatalf("publish body = %v", pub)
	}
	key := pubKey(t, rec)

	// Render injects overlay + stamps aids (author reads it).
	rec = do(t, h, http.MethodGet, "/d/"+key+"/v/1", authorHdrNoCT(), "")
	if rec.Code != 200 {
		t.Fatalf("render = %d", rec.Code)
	}
	html := rec.Body.String()
	if !strings.Contains(html, "window.__ODOC__") {
		t.Error("overlay config not injected")
	}
	if !strings.Contains(html, "data-odoc-aid=") {
		t.Error("aids not stamped")
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors") {
		t.Error("security headers missing")
	}
	// Rich inline media (video/audio, iframe embeds, self-hosted objects) must be
	// governed by explicit CSP directives, not left to default-src fallback.
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"media-src ", "frame-src ", "object-src "} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q directive: %s", want, csp)
		}
	}

	// Publish v2 auto-increments.
	rec = do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"hello","html":"<html><body><h1>Hi v2</h1></body></html>"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &pub)
	pubData, _ = pub["data"].(map[string]any)
	if pubData == nil || pubData["version"].(float64) != 2 {
		t.Fatalf("v2 version = %v", pub)
	}

	// Versions endpoint lists both (author reads).
	rec = do(t, h, http.MethodGet, "/v1/docs/"+key+"/versions", authorHdrNoCT(), "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"n":2`) {
		t.Fatalf("versions = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRenderLatestVersion(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	readAuth := authorHdrNoCT()

	rec := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"latest","html":"<html><body><h1>Version One</h1></body></html>"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish v1 = %d: %s", rec.Code, rec.Body.String())
	}
	key := pubKey(t, rec)
	rec = do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"latest","html":"<html><body><h1>Version Two</h1></body></html>"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish v2 = %d: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/d/"+key+"/v/latest", readAuth, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("render latest = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Version Two") || strings.Contains(body, "Version One") {
		t.Fatalf("latest render body = %s", body)
	}

	rec = do(t, h, http.MethodGet, "/d/"+key+"/v/latest", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unauthenticated latest render = %d; want 404: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/d/"+key+"/v/1", readAuth, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("render numeric = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Version One") || strings.Contains(body, "Version Two") {
		t.Fatalf("numeric render body = %s", body)
	}

	rec = do(t, h, http.MethodHead, "/d/"+key+"/v/Latest", readAuth, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD latest = %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD latest body length = %d; want 0", rec.Body.Len())
	}
}

func TestRenderLatestVersionNoVersions(t *testing.T) {
	h := newTestServer(t, nil)
	// Draft-first (no prior publish) can only be created by a superAdmin: draft
	// save does not stamp a creator_uid, so author-by-creator never applies here
	// and only the IsOwner short-circuit grants CapAuthor on a not-yet-existing doc.
	auth := adminHdr()
	rec := do(t, h, http.MethodPut, "/v1/docs/nover/draft", auth,
		`{"html":"<html><body><h1>draft only</h1></body></html>"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("draft save = %d: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/d/nover/v/latest", adminHdrNoCT(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("render no-version latest = %d; want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestDraftLifecycle(t *testing.T) {
	h := newTestServer(t, nil)
	// Draft-first flow: use a superAdmin identity (see TestRenderLatestVersionNoVersions
	// for why draft-first requires IsOwner rather than a creator match).
	auth := adminHdr()

	// Draft save is author-only; no credential → 404 (existence hidden).
	rec := do(t, h, http.MethodPut, "/v1/docs/dr/draft",
		map[string]string{"Content-Type": "application/json"},
		`{"html":"<html><body><h1>draft</h1></body></html>"}`)
	if rec.Code != 401 && rec.Code != 404 {
		t.Fatalf("unauthenticated draft save = %d; want 401/404", rec.Code)
	}

	// Save a draft (overwrite twice to prove it's mutable).
	var key string
	for _, body := range []string{
		`{"html":"<html><body><h1>draft one</h1></body></html>","title":"Draft Doc"}`,
		`{"html":"<html><body><h1>draft two</h1></body></html>","title":"Draft Doc"}`,
	} {
		rec = do(t, h, http.MethodPut, "/v1/docs/dr/draft", auth, body)
		if rec.Code != 200 {
			t.Fatalf("draft save = %d: %s", rec.Code, rec.Body.String())
		}
		key = pubKey(t, rec)
	}

	// The draft is NOT a version — versions endpoint has none yet (author reads).
	rec = do(t, h, http.MethodGet, "/v1/docs/"+key+"/versions", adminHdrNoCT(), "")
	if strings.Contains(rec.Body.String(), `"n":1`) {
		t.Fatalf("draft leaked into versions: %s", rec.Body.String())
	}

	// Draft render is author-only. No credential → 404 (existence hidden).
	rec = do(t, h, http.MethodGet, "/d/"+key+"/draft", nil, "")
	if rec.Code != 401 && rec.Code != 404 {
		t.Fatalf("unauthenticated draft render = %d; want 401/404", rec.Code)
	}
	rec = do(t, h, http.MethodGet, "/d/"+key+"/draft", adminHdrNoCT(), "")
	if rec.Code != 200 {
		t.Fatalf("draft render = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"mode":"draft"`) {
		t.Error("draft not rendered in draft mode")
	}
	if !strings.Contains(rec.Body.String(), "draft two") {
		t.Error("draft render shows stale content")
	}

	// Promote → the draft becomes immutable v1.
	rec = do(t, h, http.MethodPost, "/v1/docs/"+key+"/draft/promote", auth, "")
	if rec.Code != 200 {
		t.Fatalf("promote = %d: %s", rec.Code, rec.Body.String())
	}
	var pub map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &pub)
	if d, _ := pub["data"].(map[string]any); d == nil || d["version"].(float64) != 1 {
		t.Fatalf("promote body = %v; want version 1", pub)
	}

	// v1 is now committed; the author reads it, and the draft slot is cleared.
	if rec = do(t, h, http.MethodGet, "/d/"+key+"/v/1", adminHdrNoCT(), ""); rec.Code != 200 {
		t.Fatalf("published v1 render = %d", rec.Code)
	}
	rec = do(t, h, http.MethodGet, "/d/"+key+"/draft", adminHdrNoCT(), "")
	if rec.Code != 404 {
		t.Fatalf("draft after promote = %d; want 404 (cleared)", rec.Code)
	}

	// Promoting again with no draft is a clean 404, not a 500.
	rec = do(t, h, http.MethodPost, "/v1/docs/"+key+"/draft/promote", auth, "")
	if rec.Code != 404 {
		t.Fatalf("promote with no draft = %d; want 404", rec.Code)
	}
}

func TestCommentLifecycle(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	key := mustPublish(t, h, "doc", "<html><body><p>hello world</p></body></html>")

	// Create a comment (author credential).
	rec := do(t, h, http.MethodPost, "/v1/comments", auth,
		`{"slug":"`+key+`","text":"nice","version":1,"anchor":{"kind":"text","text":"hello"}}`)
	if rec.Code != 200 {
		t.Fatalf("create comment = %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	createdData, _ := created["data"].(map[string]any)
	id, _ := createdData["id"].(string)
	if id == "" {
		t.Fatalf("no comment id in %v", created)
	}

	// List shows it, wrapped in the data/pagination envelope.
	rec = do(t, h, http.MethodGet, "/v1/comments?slug="+key+"&version=1", authorHdrNoCT(), "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "nice") {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"pagination"`) || !strings.Contains(rec.Body.String(), `"created_at"`) {
		t.Fatalf("list envelope missing pagination/created_at: %s", rec.Body.String())
	}

	// React.
	rec = do(t, h, http.MethodPost, "/v1/reactions", auth,
		`{"slug":"`+key+`","comment_id":"`+id+`","emoji":"👍","version":1}`)
	if rec.Code != 200 {
		t.Fatalf("react = %d: %s", rec.Code, rec.Body.String())
	}

	// Agent reply (write-token gated) flips status.
	rec = do(t, h, http.MethodPost, "/v1/agent/replies", auth,
		`{"slug":"`+key+`","parent_id":"`+id+`","text":"done","status":"applied","applied_in":1}`)
	if rec.Code != 200 {
		t.Fatalf("agent reply = %d: %s", rec.Code, rec.Body.String())
	}

	// Delete.
	rec = do(t, h, http.MethodDelete, "/v1/comments?slug="+key+"&id="+id+"&version=1", authorHdrNoCT(), "")
	if rec.Code != 200 {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCommentMutationValidationAndAnchorRoundTrip(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	var key string
	for _, html := range []string{
		`{"slug":"anchors","html":"<html><body><p>v1</p></body></html>"}`,
		`{"slug":"anchors","html":"<html><body><p>v2</p></body></html>"}`,
	} {
		rec := do(t, h, http.MethodPost, "/v1/docs", auth, html)
		if rec.Code != http.StatusOK {
			t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
		}
		key = pubKey(t, rec)
	}

	for _, payload := range []string{
		`{"slug":"` + key + `","text":"element","version":2,"anchor":{"kind":"element","aid":"a1","selector":"p"}}`,
		`{"slug":"` + key + `","text":"text","version":"2","anchor":{"kind":"text","text":"hello","context_before":"before","context_after":"after"}}`,
		`{"slug":"` + key + `","text":"latest","version":"latest","anchor":{"kind":"element","aid":"a-latest"}}`,
		`{"slug":"` + key + `","text":"v2","version":"v2"}`,
		`{"slug":"` + key + `","text":"zero","version":0}`,
		`{"slug":"` + key + `","text":"null","version":null}`,
		`{"slug":"` + key + `","text":"omitted"}`,
	} {
		rec := do(t, h, http.MethodPost, "/v1/comments", auth, payload)
		if rec.Code != http.StatusOK {
			t.Fatalf("compatible create = %d: %s", rec.Code, rec.Body.String())
		}
	}

	rec := do(t, h, http.MethodGet, "/v1/comments?slug="+key+"&version=all", authorHdrNoCT(), "")
	var listed struct {
		Data []struct {
			ID        string         `json:"id"`
			CreatedIn int            `json:"created_in"`
			Anchor    map[string]any `json:"anchor"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 7 {
		t.Fatalf("comment count = %d, want 7: %s", len(listed.Data), rec.Body.String())
	}
	if listed.Data[0].Anchor["kind"] != "element" || listed.Data[0].Anchor["aid"] != "a1" || listed.Data[0].Anchor["selector"] != "p" {
		t.Fatalf("element anchor not preserved: %#v", listed.Data[0].Anchor)
	}
	if listed.Data[1].Anchor["kind"] != "text" || listed.Data[1].Anchor["text"] != "hello" || listed.Data[1].Anchor["context_before"] != "before" || listed.Data[1].Anchor["context_after"] != "after" {
		t.Fatalf("text anchor not preserved: %#v", listed.Data[1].Anchor)
	}
	if listed.Data[2].CreatedIn != 2 || listed.Data[2].Anchor["aid"] != "a-latest" {
		t.Fatalf("latest did not resolve to rendered latest version: %#v", listed.Data[2])
	}
	for i := 3; i < len(listed.Data); i++ {
		if listed.Data[i].Anchor != nil {
			t.Fatalf("unanchored comment %d gained anchor: %#v", i, listed.Data[i].Anchor)
		}
	}

	for _, tc := range []struct {
		method, target, payload string
	}{
		{http.MethodPost, "/v1/comments", `{"slug":"` + key + `","text":"bad","version":-1}`},
		{http.MethodPost, "/v1/comments", `{"slug":"` + key + `","text":"bad","version":"garbage"}`},
		{http.MethodPost, "/v1/comments", `{"slug":"` + key + `","text":"bad","version":2}{"anchor":{"kind":"element","aid":"a2"}}`},
		{http.MethodPatch, "/v1/comments", `{"slug":"` + key + `","id":"` + listed.Data[0].ID + `","anchor":{"kind":"element","aid":"a2"},"version":`},
		{http.MethodPatch, "/v1/comments", `{"slug":"` + key + `","id":"` + listed.Data[0].ID + `","anchor":{"kind":"element","aid":"a2"},"version":2} trailing`},
		{http.MethodPost, "/v1/reactions", `{"slug":"` + key + `","comment_id":"` + listed.Data[0].ID + `","emoji":"x","version":`},
		{http.MethodPost, "/v1/reactions", `{"slug":"` + key + `","comment_id":"` + listed.Data[0].ID + `","emoji":"x","version":2}{}`},
	} {
		rec := do(t, h, tc.method, tc.target, auth, tc.payload)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed %s = %d, want 400: %s", tc.target, rec.Code, rec.Body.String())
		}
	}

	for _, tc := range []struct {
		method, target, payload string
	}{
		{http.MethodPatch, "/v1/comments", `{"slug":"` + key + `","id":"` + listed.Data[0].ID + `","anchor":{"kind":"element","aid":"a2"},"version":"latest"}`},
		{http.MethodPost, "/v1/reactions", `{"slug":"` + key + `","comment_id":"` + listed.Data[4].ID + `","emoji":"x","version":0}`},
	} {
		rec := do(t, h, tc.method, tc.target, auth, tc.payload)
		if rec.Code != http.StatusOK {
			t.Fatalf("compatible %s = %d: %s", tc.target, rec.Code, rec.Body.String())
		}
	}
}

func TestCommentMutationRejectsFutureAndDuplicateVersions(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	pb := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"strict-versions","html":"<html><body><section>v1</section></body></html>"}`)
	if pb.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", pb.Code, pb.Body.String())
	}
	key := pubKey(t, pb)
	payloads := []string{
		`{"slug":"` + key + `","text":"future","version":999999}`,
		`{"slug":"` + key + `","text":"duplicate","version":1,"version":999999}`,
		`{"slug":"` + key + `","text":"nested duplicate","version":1,"anchor":{"kind":"element","aid":"a","aid":"b"}}`,
		`{"slug":"` + key + `","anchor":` + strings.Repeat(`[`, 10_000) + `null` + strings.Repeat(`]`, 10_000) + `}`,
	}
	for _, payload := range payloads {
		rec := do(t, h, http.MethodPost, "/v1/comments", auth, payload)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("payload %s = %d, want 400: %s", payload, rec.Code, rec.Body.String())
		}
	}
	rec := do(t, h, http.MethodGet, "/v1/comments?slug="+key+"&version=all", authorHdrNoCT(), "")
	var listed struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed.Data) != 0 {
		t.Fatalf("rejected mutations persisted: data=%s err=%v", rec.Body.String(), err)
	}
}

func TestForkExport(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	key := mustPublish(t, h, "f", "<html><body><p>content here</p></body></html>")
	_ = do(t, h, http.MethodPost, "/v1/comments", auth,
		`{"slug":"`+key+`","text":"note","version":1,"anchor":{"kind":"text","text":"content"}}`)

	rd := authorHdrNoCT()
	rec := do(t, h, http.MethodGet, "/d/"+key+"/v/1/export", rd, "")
	if rec.Code != 200 {
		t.Fatalf("export = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "octo-doc fork export") {
		t.Error("export banner missing")
	}
	if !strings.Contains(rec.Body.String(), "odoc-fork-comments") {
		t.Error("fork comments JSON missing")
	}

	rec = do(t, h, http.MethodGet, "/d/"+key+"/v/1/fork", rd, "")
	if !strings.Contains(rec.Body.String(), "window.__ODOC__") {
		t.Error("fork should boot overlay")
	}
}

func TestForkExportLatestVersion(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	rd := authorHdrNoCT()

	_ = do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"fl","html":"<html><body><p>old export</p></body></html>"}`)
	pb := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"fl","html":"<html><body><p>latest export</p></body></html>"}`)
	key := pubKey(t, pb)

	rec := do(t, h, http.MethodGet, "/d/"+key+"/v/%20LATEST%20/export", rd, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("export latest = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "latest export") || strings.Contains(body, "old export") {
		t.Fatalf("export latest body = %s", body)
	}
}

func TestBootstrapOnce(t *testing.T) {
	cfg := &config.Config{AllowBootstrap: true, MaxHTMLBytes: 1 << 20, RepoURL: "https://x", RateLimitMax: 0}
	h := newTestServer(t, cfg)
	rec := do(t, h, http.MethodPost, "/v1/admin/bootstrap", nil, "")
	if rec.Code != 200 {
		t.Fatalf("bootstrap = %d: %s", rec.Code, rec.Body.String())
	}
	// Second call conflicts.
	rec = do(t, h, http.MethodPost, "/v1/admin/bootstrap", nil, "")
	if rec.Code != 409 {
		t.Fatalf("second bootstrap = %d; want 409", rec.Code)
	}
}

func TestInvalidSlugRejected(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/v1/comments?slug=../etc", nil, "")
	if rec.Code != 400 {
		t.Fatalf("bad slug = %d; want 400", rec.Code)
	}
}

// Finding 1: on a draft-only doc (zero PUBLISHED versions) a comment created
// with version="latest" must resolve to a CONCRETE version 1 — never the latest
// sentinel (core.VersionLatest / math.MaxInt) — so the event folds correctly and
// stays draft-overlay (version 0) compatible. The same doc must accept a
// re-anchor and a reaction at "latest", and the persisted text/anchor/reaction
// must fold back at version 1.
func TestDraftOnlyLatestResolvesToConcreteVersion(t *testing.T) {
	h := newTestServer(t, nil)
	// Draft-first requires the IsOwner (superAdmin) identity; no publish happens,
	// so ListVersions returns zero versions for this slug.
	auth := adminHdr()
	if rec := do(t, h, http.MethodPut, "/v1/docs/draftonly/draft", auth,
		`{"html":"<html><body><section><p>draft body</p></section></body></html>"}`); rec.Code != http.StatusOK {
		t.Fatalf("draft save = %d: %s", rec.Code, rec.Body.String())
	}

	// Create at "latest" with an element anchor. Zero published versions ⇒ must
	// resolve to concrete v1, not math.MaxInt.
	rec := do(t, h, http.MethodPost, "/v1/comments", auth,
		`{"slug":"draftonly","text":"draft note","version":"latest","anchor":{"kind":"element","aid":"a0"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create at latest (draft-only) = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v (%s)", err, rec.Body.String())
	}
	if created.Data.ID == "" {
		t.Fatalf("no comment id: %s", rec.Body.String())
	}
	if created.Data.Version != 1 {
		t.Fatalf("created version = %d; want concrete 1 (not the latest sentinel)", created.Data.Version)
	}

	// Re-anchor (PATCH) at "latest" — also draft-only, must resolve to v1.
	rec = do(t, h, http.MethodPatch, "/v1/comments", auth,
		`{"slug":"draftonly","id":"`+created.Data.ID+`","anchor":{"kind":"element","aid":"a1"},"version":"latest"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reanchor at latest (draft-only) = %d: %s", rec.Code, rec.Body.String())
	}

	// React at "latest" — same path.
	rec = do(t, h, http.MethodPost, "/v1/reactions", auth,
		`{"slug":"draftonly","comment_id":"`+created.Data.ID+`","emoji":"👍","version":"latest"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("react at latest (draft-only) = %d: %s", rec.Code, rec.Body.String())
	}

	// Fold at the concrete version 1: the persisted text, the re-anchored aid, and
	// the reaction must all be present — proving nothing landed on math.MaxInt.
	list := do(t, h, http.MethodGet, "/v1/comments?slug=draftonly&version=1", adminHdrNoCT(), "").Body.String()
	var folded struct {
		Data []struct {
			Text      string              `json:"text"`
			Version   int                 `json:"version"`
			Anchor    map[string]any      `json:"anchor"`
			Reactions map[string][]string `json:"reactions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(list), &folded); err != nil {
		t.Fatalf("decode fold v1: %v (%s)", err, list)
	}
	if len(folded.Data) != 1 {
		t.Fatalf("fold count = %d; want 1: %s", len(folded.Data), list)
	}
	c := folded.Data[0]
	if c.Text != "draft note" {
		t.Errorf("folded text = %q; want draft note", c.Text)
	}
	if c.Version != 1 {
		t.Errorf("folded version = %d; want 1", c.Version)
	}
	if c.Anchor["kind"] != "element" || c.Anchor["aid"] != "a1" {
		t.Errorf("folded anchor = %#v; want the re-anchored element aid a1", c.Anchor)
	}
	if len(c.Reactions["👍"]) == 0 {
		t.Errorf("folded reaction missing: %#v", c.Reactions)
	}
}

// Finding 1 defense-in-depth: a numeric version equal to the latest sentinel
// (core.VersionLatest / math.MaxInt) supplied directly, quoted, or v-prefixed
// must be REJECTED at decode — only the literal string "latest" reaches the
// sentinel — so it can never be persisted as a comment's at_version.
func TestSentinelNumericVersionRejected(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	if rec := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"sentinel","html":"<html><body><p>v1</p></body></html>"}`); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	const maxInt = "9223372036854775807" // math.MaxInt on the 64-bit test platform
	for _, payload := range []string{
		`{"slug":"sentinel","text":"x","version":` + maxInt + `}`,
		`{"slug":"sentinel","text":"x","version":"` + maxInt + `"}`,
		`{"slug":"sentinel","text":"x","version":"v` + maxInt + `"}`,
	} {
		rec := do(t, h, http.MethodPost, "/v1/comments", auth, payload)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("sentinel numeric version %q = %d; want 400", payload, rec.Code)
		}
	}
}
