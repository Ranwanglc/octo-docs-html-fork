package httpx_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lml2468/octo-doc/internal/config"
	"github.com/lml2468/octo-doc/internal/platform/log"
	"github.com/lml2468/octo-doc/internal/platform/sluglock"
	"github.com/lml2468/octo-doc/internal/service"
	"github.com/lml2468/octo-doc/internal/storage/memory"
	"github.com/lml2468/octo-doc/internal/transport/httpx"
)

// FEAT-3/A capability tests: octo trust-header identity + doc_binding channel
// must map to CapReader (visible) or CapAuthor (binding creator), and
// hidden-404 / errors must fall through cleanly to the share-code path.
//
// The doc_binding client still needs the X-Octo-Token module token (OCT-144
// channel) to authenticate to octo-server — that stays untouched by 方案 C.
// What 方案 C changes is only the identity source (three trust headers instead
// of userinfo).

// stubBindingFetcher is a table-driven octo-server stub keyed by (token, slug).
type stubBindingFetcher struct {
	byKey map[string]*service.DocBindingInfo
	// errKeys triggers a fetcher error for the given (token, slug); used to
	// prove flaky octo does not fail the doc request.
	errKeys map[string]bool
	calls   int
}

func (s *stubBindingFetcher) Fetch(_ context.Context, token, slug string) (*service.DocBindingInfo, error) {
	s.calls++
	k := token + "|" + slug
	if s.errKeys[k] {
		return nil, http.ErrHandlerTimeout
	}
	return s.byKey[k], nil
}

// newTestServerWithBinding wires a server with the FEAT-3 doc_binding channel.
// Identity arrives via the trust headers on each request; the module token
// (X-Octo-Token) is forwarded to the binding stub.
func newTestServerWithBinding(t *testing.T, binding service.BindingFetcher) http.Handler {
	t.Helper()
	cfg := &config.Config{
		WriteToken:        "test-token",
		LoginEnabled:      true,
		MaxHTMLBytes:      5 << 20,
		MaxAssetBytes:     25 << 20,
		RepoURL:           "https://example.com/repo",
		AssetMIMEAllow:    []string{"image/png"},
		OctoDocBindingURL: "http://octo.invalid",
		OctoDocBindingTTL: time.Minute,
	}
	store := memory.New()
	locker := sluglock.NewMemory()
	comments := service.NewCommentService(store, locker)
	docs := service.NewDocService(store, store, comments, locker, cfg.BaseURL, cfg.MaxHTMLBytes)
	assets := service.NewAssetService(store, store, locker, cfg.MaxAssetBytes, cfg.AssetMIMEAllow)
	auth := service.NewAuthService(store, cfg, locker)
	docBinding := service.NewDocBindingClient(binding, cfg.OctoDocBindingTTL)
	srv := httpx.New(httpx.Deps{
		Config: cfg, Logger: log.New("silent"), Docs: docs, Comments: comments,
		Assets: assets, Auth: auth, DocBinding: docBinding,
		OverlayJS: "/* overlay */",
	})
	return srv.Handler()
}

// proxiedMember shorthand: trust-header identity for a role=member caller
// plus the module token used to key the binding stub.
func proxiedMember(uid, tok string) map[string]string {
	return map[string]string{
		"X-Octo-Uid":   uid,
		"X-Octo-Name":  uid,
		"X-Octo-Role":  "member",
		"X-Octo-Token": tok,
	}
}

func proxiedSuperAdmin(uid, tok string) map[string]string {
	return map[string]string{
		"X-Octo-Uid":   uid,
		"X-Octo-Name":  uid,
		"X-Octo-Role":  "superAdmin",
		"X-Octo-Token": tok,
	}
}

// FEAT-3 §hook: non-superAdmin identity + doc_binding creator match →
// CapAuthor. Author-only op (rotate share) must succeed with no share code.
func TestDocBindingCreatorGrantsAuthor(t *testing.T) {
	bf := &stubBindingFetcher{byKey: map[string]*service.DocBindingInfo{
		"tok-creator|docG": {Slug: "docG", MountType: "group", GroupNo: "g1", CreatorUID: "u-creator"},
	}}
	h := newTestServerWithBinding(t, bf)
	publish(t, h, "docG")

	rec := do(t, h, http.MethodPost, "/v1/docs/docG/share",
		proxiedMember("u-creator", "tok-creator"), "")
	if rec.Code != 200 {
		t.Fatalf("binding creator share rotate = %d: %s", rec.Code, rec.Body.String())
	}
}

// FEAT-3 §hook: non-superAdmin identity + visible binding but not creator
// → CapReader. A comment (reader-gate) succeeds without any share code.
func TestDocBindingMemberGrantsReader(t *testing.T) {
	bf := &stubBindingFetcher{byKey: map[string]*service.DocBindingInfo{
		"tok-member|docH": {Slug: "docH", MountType: "group", GroupNo: "g1", CreatorUID: "u-other"},
	}}
	h := newTestServerWithBinding(t, bf)
	publish(t, h, "docH")

	rec := do(t, h, http.MethodGet, "/d/docH/v/1",
		proxiedMember("u-member", "tok-member"), "")
	if rec.Code != 200 {
		t.Fatalf("binding member render = %d; want 200 (CapReader)", rec.Code)
	}

	rec = do(t, h, http.MethodPost, "/v1/comments",
		merge(proxiedMember("u-member", "tok-member"), map[string]string{"Content-Type": "application/json"}),
		`{"slug":"docH","text":"hi","version":1,"anchor":{"kind":"text","text":"hello"}}`)
	if rec.Code != 200 {
		t.Fatalf("binding member comment = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"u-member"`) {
		t.Errorf("expected octo uid u-member in comment: %s", rec.Body.String())
	}

	// A binding-derived reader must NOT be able to hit author-only ops —
	// otherwise the reader mapping would leak write authority.
	rec = do(t, h, http.MethodPost, "/v1/docs/docH/share",
		proxiedMember("u-member", "tok-member"), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reader-binding share rotate = %d; want 404 (must not upgrade to author)", rec.Code)
	}
}

// FEAT-3 §hook: hidden-404 from octo-server → no cap here. Non-member falls
// through to the "no credential" path (404, not 5xx).
func TestDocBindingHiddenNotFoundFallsThrough(t *testing.T) {
	bf := &stubBindingFetcher{byKey: map[string]*service.DocBindingInfo{}}
	h := newTestServerWithBinding(t, bf)
	publish(t, h, "docI")

	rec := do(t, h, http.MethodGet, "/d/docI/v/1",
		proxiedMember("u-outsider", "tok-outsider"), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("hidden-binding render = %d; want 404 (no cap)", rec.Code)
	}
	if bf.calls == 0 {
		t.Fatal("doc_binding probe not invoked; middleware wiring broken")
	}
}

// FEAT-3 §hook: octo-server error (5xx / timeout) must not fail the doc
// request. The share-code fallback still works.
func TestDocBindingErrorFallsThroughToShareCode(t *testing.T) {
	bf := &stubBindingFetcher{
		byKey:   map[string]*service.DocBindingInfo{},
		errKeys: map[string]bool{"tok-flaky|docJ": true},
	}
	h := newTestServerWithBinding(t, bf)
	publish(t, h, "docJ")
	code := generateShareCode(t, h, "docJ")

	rec := do(t, h, http.MethodGet, "/d/docJ/v/1",
		merge(proxiedMember("u-flaky", "tok-flaky"),
			map[string]string{"Authorization": "Bearer " + code}), "")
	if rec.Code != 200 {
		t.Fatalf("flaky-binding + share code render = %d: %s", rec.Code, rec.Body.String())
	}
}

// FEAT-3 §hook: superAdmin already gets CapAuthor via trust headers — the
// binding probe must not fire for them (saves an octo-server round trip and
// matches the "superAdmin short-circuits" comment in bestCred).
func TestDocBindingSkipsForSuperAdmin(t *testing.T) {
	bf := &stubBindingFetcher{byKey: map[string]*service.DocBindingInfo{}}
	h := newTestServerWithBinding(t, bf)
	publish(t, h, "docK")

	rec := do(t, h, http.MethodGet, "/d/docK/v/1",
		proxiedSuperAdmin("u-admin", "tok-admin"), "")
	if rec.Code != 200 {
		t.Fatalf("admin render = %d", rec.Code)
	}
	if bf.calls != 0 {
		t.Fatalf("doc_binding must not be probed for superAdmin; calls=%d", bf.calls)
	}
}

// FEAT-3 §hook: a request with no X-Octo-Token must not probe doc_binding —
// there is no module token to attach, and hitting octo unauthenticated would
// 401 on every anonymous render. This keeps anonymous request cost unchanged.
func TestDocBindingSkipsWithoutOctoToken(t *testing.T) {
	bf := &stubBindingFetcher{byKey: map[string]*service.DocBindingInfo{}}
	h := newTestServerWithBinding(t, bf)
	publish(t, h, "docL")
	code := generateShareCode(t, h, "docL")

	// Share-code-only request: doc_binding must not be hit.
	rec := do(t, h, http.MethodGet, "/d/docL/v/1",
		map[string]string{"Authorization": "Bearer " + code}, "")
	if rec.Code != 200 {
		t.Fatalf("share-code render = %d", rec.Code)
	}
	if bf.calls != 0 {
		t.Fatalf("doc_binding probed with no octo token; calls=%d", bf.calls)
	}
}

// merge overlays extras onto base; keeps callers concise.
func merge(base map[string]string, extras ...map[string]string) map[string]string {
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
