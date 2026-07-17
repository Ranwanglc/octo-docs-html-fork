package httpx_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strconv"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
)

// gifBytes is a valid GIF89a header — http.DetectContentType classifies it as
// image/gif, which the test server's allowlist permits.
var gifBytes = []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")

// multipartFile builds a multipart body with a single "file" part and returns the
// body bytes plus the Content-Type header (which carries the boundary).
func multipartFile(t *testing.T, filename string, data []byte) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), mw.FormDataContentType()
}

func TestAssetLifecycle(t *testing.T) {
	h := newTestServer(t, nil)
	authJSON := authorHdr()

	// Publish a doc so it exists (and to mint a share code from).
	if rec := do(t, h, http.MethodPost, "/v1/docs", authJSON,
		`{"slug":"pics","html":"<html><body><p>gallery</p></body></html>"}`); rec.Code != 200 {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}

	// Upload an asset (author, multipart).
	body, ct := multipartFile(t, "cat.gif", gifBytes)
	rec := do(t, h, http.MethodPost, "/v1/docs/pics/assets",
		map[string]string{octoUIDHeaderName: testUID, "Content-Type": ct}, body)
	if rec.Code != 200 {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body.String())
	}
	var up map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &up)
	upData, _ := up["data"].(map[string]any)
	sha, _ := upData["sha256"].(string)
	if len(sha) != 64 {
		t.Fatalf("upload sha = %q; want 64 hex chars (%v)", sha, up)
	}
	if upData["mime"] != "image/gif" {
		t.Errorf("mime = %v; want image/gif", upData["mime"])
	}

	// Mint a share code for reader-capability access.
	rec = do(t, h, http.MethodPost, "/v1/docs/pics/share", authorHdrNoCT(), "")
	if rec.Code != 200 {
		t.Fatalf("share = %d: %s", rec.Code, rec.Body.String())
	}
	var sh map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &sh)
	shData, _ := sh["data"].(map[string]any)
	code, _ := shData["code"].(string)
	if code == "" {
		t.Fatalf("no share code in %v", sh)
	}

	// List assets with the reader code (Bearer).
	rec = do(t, h, http.MethodGet, "/v1/docs/pics/assets", map[string]string{"Authorization": "Bearer " + code}, "")
	if rec.Code != 200 {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(sha)) {
		t.Errorf("list missing uploaded asset: %s", rec.Body.String())
	}

	// Serve the raw bytes with the reader code as Bearer (the agent/CLI transport;
	// no cookie-exchange redirect on this path).
	rec = do(t, h, http.MethodGet, "/d/pics/assets/"+sha, map[string]string{"Authorization": "Bearer " + code}, "")
	if rec.Code != 200 {
		t.Fatalf("serve = %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), gifBytes) {
		t.Error("served bytes differ from uploaded")
	}
	if got := rec.Header().Get("Content-Type"); got != "image/gif" {
		t.Errorf("Content-Type = %q; want image/gif", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff missing: %q", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
		t.Errorf("locked CSP missing: %q", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q; want DENY", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q; want no-referrer", got)
	}
	if got := rec.Header().Get("Cache-Control"); got == "" || !bytes.Contains([]byte(got), []byte("immutable")) {
		t.Errorf("immutable cache missing: %q", got)
	}

	// A browser's first hit carries ?code= and gets the 302 cookie-exchange (the
	// code leaves the URL). This mirrors the version-render reader flow.
	rec = do(t, h, http.MethodGet, "/d/pics/assets/"+sha+"?code="+code, nil, "")
	if rec.Code != 302 {
		t.Fatalf("browser first-hit serve = %d; want 302 cookie exchange", rec.Code)
	}

	// A request with no credential must 404 (existence hidden), not serve bytes.
	if rec := do(t, h, http.MethodGet, "/d/pics/assets/"+sha, nil, ""); rec.Code != 404 {
		t.Fatalf("uncredentialed serve = %d; want 404", rec.Code)
	}

	// A reader cannot delete (author-only) — hidden as 404.
	if rec := do(t, h, http.MethodDelete, "/v1/docs/pics/assets/"+sha,
		map[string]string{"Authorization": "Bearer " + code}, ""); rec.Code != 404 {
		t.Fatalf("reader delete = %d; want 404", rec.Code)
	}

	// Author deletes it.
	if rec := do(t, h, http.MethodDelete, "/v1/docs/pics/assets/"+sha,
		authorHdrNoCT(), ""); rec.Code != 200 {
		t.Fatalf("author delete = %d: %s", rec.Code, rec.Body.String())
	}
	// Now gone.
	if rec := do(t, h, http.MethodGet, "/d/pics/assets/"+sha,
		map[string]string{"Authorization": "Bearer " + code}, ""); rec.Code != 404 {
		t.Fatalf("serve after delete = %d; want 404", rec.Code)
	}
}

func TestAssetUploadRejectsDisallowedType(t *testing.T) {
	h := newTestServer(t, nil)
	authJSON := authorHdr()
	_ = do(t, h, http.MethodPost, "/v1/docs", authJSON,
		`{"slug":"d","html":"<html><body><p>x</p></body></html>"}`)

	// A plain-text payload sniffs as text/plain, which is not in the allowlist.
	body, ct := multipartFile(t, "notreally.png", []byte("this is just text, not an image"))
	rec := do(t, h, http.MethodPost, "/v1/docs/d/assets",
		map[string]string{octoUIDHeaderName: testUID, "Content-Type": ct}, body)
	if rec.Code != 400 {
		t.Fatalf("upload text = %d; want 400: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("unsupported_media_type")) {
		t.Errorf("want unsupported_media_type: %s", rec.Body.String())
	}
}

func TestAssetUploadRequiresAuthor(t *testing.T) {
	h := newTestServer(t, nil)
	authJSON := authorHdr()
	_ = do(t, h, http.MethodPost, "/v1/docs", authJSON,
		`{"slug":"d","html":"<html><body><p>x</p></body></html>"}`)

	// No credential → author-only upload route hides as 404.
	body, ct := multipartFile(t, "cat.gif", gifBytes)
	rec := do(t, h, http.MethodPost, "/v1/docs/d/assets", map[string]string{"Content-Type": ct}, body)
	if rec.Code != 404 {
		t.Fatalf("uncredentialed upload = %d; want 404: %s", rec.Code, rec.Body.String())
	}
}

// uploadAssetForRange publishes a doc, uploads an asset, and returns the sha and a
// reader share code — the setup shared by the Range tests.
func uploadAssetForRange(t *testing.T, h http.Handler, slug string, data []byte) (sha, code string) {
	t.Helper()
	authJSON := authorHdr()
	if rec := do(t, h, http.MethodPost, "/v1/docs", authJSON,
		`{"slug":"`+slug+`","html":"<html><body><p>x</p></body></html>"}`); rec.Code != 200 {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	body, ct := multipartFile(t, "clip.gif", data)
	rec := do(t, h, http.MethodPost, "/v1/docs/"+slug+"/assets",
		map[string]string{octoUIDHeaderName: testUID, "Content-Type": ct}, body)
	if rec.Code != 200 {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body.String())
	}
	var up map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &up)
	sha, _ = up["data"].(map[string]any)["sha256"].(string)

	rec = do(t, h, http.MethodPost, "/v1/docs/"+slug+"/share", authorHdrNoCT(), "")
	var sh map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &sh)
	code, _ = sh["data"].(map[string]any)["code"].(string)
	if sha == "" || code == "" {
		t.Fatalf("setup failed: sha=%q code=%q", sha, code)
	}
	return sha, code
}

func TestAssetServeRangeRequest(t *testing.T) {
	h := newTestServer(t, nil)
	// A payload long enough to slice; still sniffs as image/gif via the header.
	data := append([]byte(nil), gifBytes...)
	data = append(data, []byte("0123456789ABCDEF0123456789")...)
	sha, code := uploadAssetForRange(t, h, "vid", data)

	// Request bytes 5-9 (inclusive) → 5 bytes, 206 Partial Content.
	rec := do(t, h, http.MethodGet, "/d/vid/assets/"+sha,
		map[string]string{"Authorization": "Bearer " + code, "Range": "bytes=5-9"}, "")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range GET = %d; want 206", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), data[5:10]) {
		t.Errorf("range body = %q; want %q", rec.Body.Bytes(), data[5:10])
	}
	if cr := rec.Header().Get("Content-Range"); cr == "" || !bytes.Contains([]byte(cr), []byte("/"+strconv.Itoa(len(data)))) {
		t.Errorf("Content-Range = %q; want a range over total %d", cr, len(data))
	}
	// The locked-down headers must still be present on a partial response.
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
		t.Errorf("locked CSP missing on 206: %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff missing on 206: %q", got)
	}
}

func TestAssetServeAdvertisesAcceptRanges(t *testing.T) {
	h := newTestServer(t, nil)
	data := append([]byte(nil), gifBytes...)
	data = append(data, []byte("more-bytes-here")...)
	sha, code := uploadAssetForRange(t, h, "vid2", data)

	// A full GET advertises range support so players know they can seek.
	rec := do(t, h, http.MethodGet, "/d/vid2/assets/"+sha,
		map[string]string{"Authorization": "Bearer " + code}, "")
	if rec.Code != 200 {
		t.Fatalf("full GET = %d; want 200", rec.Code)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q; want bytes", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Error("full GET body differs")
	}
}

func TestAssetServeUnsatisfiableRange(t *testing.T) {
	h := newTestServer(t, nil)
	data := append([]byte(nil), gifBytes...)
	sha, code := uploadAssetForRange(t, h, "vid3", data)

	// A range past the end → 416 Range Not Satisfiable.
	rec := do(t, h, http.MethodGet, "/d/vid3/assets/"+sha,
		map[string]string{"Authorization": "Bearer " + code, "Range": "bytes=99999-"}, "")
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("out-of-range GET = %d; want 416", rec.Code)
	}
}

// pdfBytes is a minimal PDF header — http.DetectContentType classifies it as
// application/pdf.
var pdfBytes = []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n%%EOF\n")

// TestAssetPDFEmbedPath verifies the P2.3 inline-PDF story end to end: a PDF
// uploads (application/pdf is allowlisted), lists, and serves with the correct
// Content-Type under the locked-down asset CSP — so a doc can embed it via
// <object data="/d/<slug>/assets/<sha>" type="application/pdf">. The document
// render CSP already permits object-src 'self', which covers the same-origin URL.
func TestAssetPDFEmbedPath(t *testing.T) {
	cfg := &config.Config{
		WriteToken: "test-token", MaxHTMLBytes: 5 << 20, RepoURL: "https://example.com/repo",
		RateLimitMax:   0,
		MaxAssetBytes:  25 << 20,
		AssetMIMEAllow: []string{"application/pdf"},
	}
	h := newTestServer(t, cfg)
	authJSON := authorHdr()
	if rec := do(t, h, http.MethodPost, "/v1/docs", authJSON,
		`{"slug":"paper","html":"<html><body><p>doc</p></body></html>"}`); rec.Code != 200 {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}

	// Upload the PDF (author).
	body, ct := multipartFile(t, "report.pdf", pdfBytes)
	rec := do(t, h, http.MethodPost, "/v1/docs/paper/assets",
		map[string]string{octoUIDHeaderName: testUID, "Content-Type": ct}, body)
	if rec.Code != 200 {
		t.Fatalf("upload pdf = %d: %s", rec.Code, rec.Body.String())
	}
	var up map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &up)
	upData, _ := up["data"].(map[string]any)
	sha, _ := upData["sha256"].(string)
	if upData["mime"] != "application/pdf" {
		t.Fatalf("pdf mime = %v; want application/pdf", upData["mime"])
	}

	// Mint a reader code and fetch it back.
	rec = do(t, h, http.MethodPost, "/v1/docs/paper/share", authorHdrNoCT(), "")
	var sh map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &sh)
	code, _ := sh["data"].(map[string]any)["code"].(string)

	rec = do(t, h, http.MethodGet, "/d/paper/assets/"+sha, map[string]string{"Authorization": "Bearer " + code}, "")
	if rec.Code != 200 {
		t.Fatalf("serve pdf = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("served Content-Type = %q; want application/pdf", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), pdfBytes) {
		t.Error("served PDF bytes differ from uploaded")
	}
	// Even a PDF is served sandboxed / no-exec — the viewer treats it as data.
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
		t.Errorf("locked CSP missing on pdf: %q", got)
	}
}
