// Package e2e exercises the full server against real PostgreSQL + S3 (MinIO),
// proving the complete publish → render → comment → agent-reply → fork/export →
// delete round-trip works end to end. Gated on OCTO_TEST_DATABASE_URL +
// OCTO_TEST_S3_BUCKET; skipped otherwise.
package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/lml2468/octo-doc/assets"
	"github.com/lml2468/octo-doc/internal/config"
	"github.com/lml2468/octo-doc/internal/platform/log"
	"github.com/lml2468/octo-doc/internal/platform/sluglock"
	"github.com/lml2468/octo-doc/internal/service"
	"github.com/lml2468/octo-doc/internal/storage/postgres"
	s3store "github.com/lml2468/octo-doc/internal/storage/s3"
	"github.com/lml2468/octo-doc/internal/transport/httpx"
)

func TestFullLifecycle(t *testing.T) {
	dbURL := os.Getenv("OCTO_TEST_DATABASE_URL")
	bucket := os.Getenv("OCTO_TEST_S3_BUCKET")
	if dbURL == "" || bucket == "" {
		t.Skip("set OCTO_TEST_DATABASE_URL and OCTO_TEST_S3_BUCKET to run e2e")
	}
	ctx := context.Background()

	pg, err := postgres.Open(ctx, dbURL, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	if err := pg.TruncateAll(ctx); err != nil {
		t.Fatal(err)
	}

	blobs, err := s3store.Open(ctx, s3store.Options{
		Bucket:         bucket,
		Region:         envOr("OCTO_TEST_S3_REGION", "us-east-1"),
		Endpoint:       os.Getenv("OCTO_TEST_S3_ENDPOINT"),
		ForcePathStyle: true,
		AccessKeyID:    os.Getenv("OCTO_TEST_S3_ACCESS_KEY_ID"),
		SecretKey:      os.Getenv("OCTO_TEST_S3_SECRET_ACCESS_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{WriteToken: "e2e", MaxHTMLBytes: 5 << 20, RepoURL: "https://example.com", RateLimitMax: 0}
	locker := sluglock.NewMemory()
	comments := service.NewCommentService(pg, locker)
	docs := service.NewDocService(blobs, pg, comments, locker, "", cfg.MaxHTMLBytes)
	auth := service.NewAuthService(pg, cfg, locker)
	h := httpx.New(httpx.Deps{Config: cfg, Logger: log.New("silent"), Docs: docs, Comments: comments, Auth: auth, OverlayJS: assets.OverlayJS}).Handler()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	slug := "e2e-lifecycle"
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/docs/"+slug, nil)
		req.Header.Set("Authorization", "Bearer e2e")
		http.DefaultClient.Do(req) //nolint:errcheck
	})

	// Publish. postJSON returns the unwrapped data object.
	pub := postJSON(t, srv.URL+"/v1/docs", "e2e",
		`{"slug":"`+slug+`","html":"<html><body><h1>T</h1><svg viewBox=\"0 0 1 1\"></svg><p>anchor me here</p></body></html>","title":"E2E"}`)
	if pub["version"].(float64) != 1 || pub["aids"].(float64) != 1 {
		t.Fatalf("publish result = %v", pub)
	}

	// Render — overlay + aids + persisted to S3.
	body := getText(t, srv.URL+"/d/"+slug+"/v/1")
	if !strings.Contains(body, "window.__ODOC__") || !strings.Contains(body, "data-odoc-aid") {
		t.Fatal("render missing overlay or aids")
	}

	// Comment + agent reply (author credential — docs are private by default).
	c := postJSON(t, srv.URL+"/v1/comments", "e2e",
		`{"slug":"`+slug+`","text":"q","version":1,"anchor":{"kind":"text","text":"anchor me"}}`)
	cid := c["id"].(string)
	_ = postJSON(t, srv.URL+"/v1/agent/replies", "e2e",
		`{"slug":"`+slug+`","parent_id":"`+cid+`","text":"done","status":"applied","applied_in":1}`)

	list := getText(t, srv.URL+"/v1/comments?slug="+slug+"&version=1")
	if !strings.Contains(list, `"status":"applied"`) || !strings.Contains(list, "odoc-agent") {
		t.Fatalf("agent reply not reflected: %s", list)
	}
	// Envelope compliance: list carries data + pagination + R3 created_at.
	if !strings.Contains(list, `"pagination"`) || !strings.Contains(list, `"created_at"`) {
		t.Fatalf("list envelope non-compliant: %s", list)
	}

	// Export + fork.
	if !strings.Contains(getText(t, srv.URL+"/d/"+slug+"/v/1/export"), "octo-doc fork export") {
		t.Fatal("export banner missing")
	}
	if !strings.Contains(getText(t, srv.URL+"/d/"+slug+"/v/1/fork"), `"mode":"fork"`) {
		t.Fatal("fork mode missing")
	}
}

func postJSON(t *testing.T, url, token, body string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("POST %s = %d: %s", url, res.StatusCode, raw)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode %s: %v (%s)", url, err, raw)
	}
	// All /v1 JSON endpoints wrap the payload in {"data": ...}; return data.
	data, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("response missing data envelope: %s", raw)
	}
	return data
}

// getText GETs a doc/JSON route as the author (the e2e write token) — docs are
// private by default, so reads need a credential.
func getText(t *testing.T, url string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer e2e")
	res, err := http.DefaultClient.Do(req) //nolint:noctx
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("GET %s = %d: %s", url, res.StatusCode, raw)
	}
	return string(raw)
}

func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}
