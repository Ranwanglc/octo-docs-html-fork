package httpx_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/log"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/transport/httpx"
)

func TestExplicitCanonicalCreateHasNoDocRefAndReturnsDocIDAsSlug(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "publisher-bot", botName: "Publisher", botSpaceID: "space-1", botOwnerUID: "owner-1"})
	var got map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"docId": "doc-42", "octoDocSlug": "doc-42", "shareUrl": "https://docs.test/d/doc-42", "created": true})
	}))
	defer backend.Close()
	cfg := &config.Config{BaseURL: "https://html.test", MaxHTMLBytes: 5 << 20, MaxAssetBytes: 25 << 20, AssetMIMEAllow: []string{"image/png"}, BotAuthEnabled: true, OctoServerBaseURL: "http://octo.test"}
	store := memory.New()
	lock := sluglock.NewMemory()
	comments := service.NewCommentService(store, lock)
	docs := service.NewDocService(store, store, comments, lock, cfg.BaseURL, cfg.MaxHTMLBytes).WithDocsBackendRegistration(docsbackend.New(backend.URL, "process-token", nil), nil)
	h := httpx.New(httpx.Deps{Config: cfg, Logger: log.New("silent"), Docs: docs, Comments: comments, Auth: service.NewAuthService(store, cfg, lock), OverlayJS: "x"}).Handler()
	rec := do(t, h, http.MethodPost, "/v1/docs", map[string]string{"Authorization": "Bearer publisher-token", "Content-Type": "application/json"}, `{"idempotency_key":"create-1","html":"<html>x</html>"}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got["idempotencyKey"] != "create-1" || got["octoDocSlug"] != nil || got["htmlAlias"] != nil {
		t.Fatalf("backend body=%v", got)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Data["doc_id"] != "doc-42" || env.Data["slug"] != "doc-42" {
		t.Fatalf("response=%v", env.Data)
	}
}

func TestExplicitCanonicalDraftCreateReturnsCreated(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "publisher-bot", botName: "Publisher", botSpaceID: "space-1", botOwnerUID: "owner-1"})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"docId": "doc-draft", "octoDocSlug": "doc-draft", "shareUrl": "https://docs.test/d/doc-draft", "created": true})
	}))
	defer backend.Close()
	cfg := &config.Config{BaseURL: "https://html.test", MaxHTMLBytes: 5 << 20, MaxAssetBytes: 25 << 20, AssetMIMEAllow: []string{"image/png"}, BotAuthEnabled: true, OctoServerBaseURL: "http://octo.test"}
	store := memory.New()
	lock := sluglock.NewMemory()
	comments := service.NewCommentService(store, lock)
	docs := service.NewDocService(store, store, comments, lock, cfg.BaseURL, cfg.MaxHTMLBytes).WithDocsBackendRegistration(docsbackend.New(backend.URL, "process-token", nil), nil)
	h := httpx.New(httpx.Deps{Config: cfg, Logger: log.New("silent"), Docs: docs, Comments: comments, Auth: service.NewAuthService(store, cfg, lock), OverlayJS: "x"}).Handler()

	rec := do(t, h, http.MethodPost, "/v1/docs/draft", map[string]string{"Authorization": "Bearer publisher-token", "Content-Type": "application/json"}, `{"idempotency_key":"draft-1","html":"<html>x</html>"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
}

func TestExplicitCanonicalDraftRejectsMissingHTMLBeforeRegistrationOrWrite(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "publisher-bot", botName: "Publisher", botSpaceID: "space-1", botOwnerUID: "owner-1"})
	var registrations int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		registrations++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()
	cfg := &config.Config{BaseURL: "https://html.test", MaxHTMLBytes: 5 << 20, MaxAssetBytes: 25 << 20, AssetMIMEAllow: []string{"image/png"}, BotAuthEnabled: true, OctoServerBaseURL: "http://octo.test"}

	for _, tc := range []struct{ name, body string }{{"missing", `{"idempotency_key":"draft-missing"}`}, {"empty", `{"idempotency_key":"draft-empty","html":""}`}} {
		t.Run(tc.name, func(t *testing.T) {
			store := memory.New()
			lock := sluglock.NewMemory()
			comments := service.NewCommentService(store, lock)
			docs := service.NewDocService(store, store, comments, lock, cfg.BaseURL, cfg.MaxHTMLBytes).WithDocsBackendRegistration(docsbackend.New(backend.URL, "process-token", nil), nil)
			h := httpx.New(httpx.Deps{Config: cfg, Logger: log.New("silent"), Docs: docs, Comments: comments, Auth: service.NewAuthService(store, cfg, lock), OverlayJS: "x"}).Handler()
			rec := do(t, h, http.MethodPost, "/v1/docs/draft", map[string]string{"Authorization": "Bearer publisher-token", "Content-Type": "application/json"}, tc.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"html_required"`) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if registrations != 0 {
				t.Fatalf("registrar calls=%d, want 0", registrations)
			}
			if entries, err := store.ListMeta(t.Context()); err != nil || len(entries) != 0 {
				t.Fatalf("local metadata=%v err=%v", entries, err)
			}
		})
	}
}
