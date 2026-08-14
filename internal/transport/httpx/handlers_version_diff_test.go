package httpx_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// publishVersion publishes html under slug and returns nothing; it fails the
// test on a non-200 so callers can assume the version exists.
func publishVersion(t *testing.T, h http.Handler, slug, html string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"slug": slug, "html": html})
	if err != nil {
		t.Fatalf("marshal publish body: %v", err)
	}
	rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), string(body))
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("publish %s status = %d: %s", slug, rec.Code, rec.Body.String())
	}
}

func decodeDiffData(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v (%s)", err, body)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no data object: %s", body)
	}
	return data
}

func TestVersionSourceReturnsStoredBytesAsInertText(t *testing.T) {
	h := newTestServer(t, nil)
	const html = `<html><body><p>hello</p></body></html>`
	publishVersion(t, h, "srcdoc", html)

	rec := do(t, h, http.MethodGet, "/v1/docs/srcdoc/versions/1/source", authorHdrNoCT(), "")
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	// The response must be the stored bytes: stamped, but with no overlay script
	// and no signed asset rewriting from the render path.
	if !strings.Contains(rec.Body.String(), "<p") || !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("body does not look like the published document: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "__ODOC__") {
		t.Fatalf("overlay leaked into the source response")
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q; want inert text/plain", got)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"X-Document-Version":      "1",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Fatalf("%s = %q; want %q", header, got, want)
		}
	}
	// Publish accepts a caller-supplied version number and overwrites, so a
	// numbered version is not write-once and must not be cached as immutable.
	cacheControl := rec.Header().Get("Cache-Control")
	if strings.Contains(cacheControl, "immutable") {
		t.Fatalf("Cache-Control = %q; version N is overwritable and must revalidate", cacheControl)
	}
	if !strings.Contains(cacheControl, "private") || !strings.Contains(cacheControl, "no-cache") {
		t.Fatalf("Cache-Control = %q; want private and revalidating", cacheControl)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Expose-Headers"), "X-Document-Version") {
		t.Fatalf("version header is not exposed cross-origin")
	}
}

func TestVersionSourceLatestIsNotCachedImmutable(t *testing.T) {
	h := newTestServer(t, nil)
	publishVersion(t, h, "latestdoc", `<html><body><p>a</p></body></html>`)
	publishVersion(t, h, "latestdoc", `<html><body><p>b</p></body></html>`)

	rec := do(t, h, http.MethodGet, "/v1/docs/latestdoc/versions/latest/source", authorHdrNoCT(), "")
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Document-Version"); got != "2" {
		t.Fatalf("latest resolved to version %q; want 2", got)
	}
	if strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("latest must not be cached as immutable: %q", rec.Header().Get("Cache-Control"))
	}
}

// A republish over an existing version number changes the bytes, so the ETag
// must change with them — this is why the route cannot advertise immutability.
func TestVersionSourceETagTracksOverwrittenVersion(t *testing.T) {
	h := newTestServer(t, nil)
	publishVersion(t, h, "overwritten", `<html><body><p>original</p></body></html>`)
	first := do(t, h, http.MethodGet, "/v1/docs/overwritten/versions/1/source", authorHdrNoCT(), "")

	body, err := json.Marshal(map[string]any{"slug": "overwritten", "html": `<html><body><p>REPLACED</p></body></html>`, "version": 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), string(body)); rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("republish status = %d: %s", rec.Code, rec.Body.String())
	}

	headers := authorHdrNoCT()
	headers["If-None-Match"] = first.Header().Get("ETag")
	second := do(t, h, http.MethodGet, "/v1/docs/overwritten/versions/1/source", headers, "")
	if second.Code == http.StatusNotModified {
		t.Fatalf("stale ETag matched after the version was overwritten")
	}
	if !strings.Contains(second.Body.String(), "REPLACED") {
		t.Fatalf("body = %q; want the new bytes", second.Body.String())
	}
}

func TestVersionSourceHonoursIfNoneMatch(t *testing.T) {
	h := newTestServer(t, nil)
	publishVersion(t, h, "etagdoc", `<html><body><p>a</p></body></html>`)

	first := do(t, h, http.MethodGet, "/v1/docs/etagdoc/versions/1/source", authorHdrNoCT(), "")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("no ETag on the first response")
	}
	headers := authorHdrNoCT()
	headers["If-None-Match"] = etag
	second := do(t, h, http.MethodGet, "/v1/docs/etagdoc/versions/1/source", headers, "")
	if second.Code != http.StatusNotModified {
		t.Fatalf("status = %d; want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 carried a body: %s", second.Body.String())
	}
}

func TestVersionSourceRejectsBadVersionAndMissingDoc(t *testing.T) {
	h := newTestServer(t, nil)
	publishVersion(t, h, "reject", `<html><body><p>a</p></body></html>`)

	for _, version := range []string{"0", "-1", "1.5", "abc", "%20"} {
		rec := do(t, h, http.MethodGet, "/v1/docs/reject/versions/"+version+"/source", authorHdrNoCT(), "")
		if rec.Code != 400 && rec.Code != 404 {
			t.Fatalf("version %q status = %d; want a rejection", version, rec.Code)
		}
	}
	rec := do(t, h, http.MethodGet, "/v1/docs/reject/versions/99/source", authorHdrNoCT(), "")
	if rec.Code != 404 {
		t.Fatalf("missing version status = %d; want 404", rec.Code)
	}
}

func TestVersionSourceRequiresReadAccess(t *testing.T) {
	h := newTestServer(t, nil)
	publishVersion(t, h, "privdoc", `<html><body><p>secret</p></body></html>`)

	// The repo hides an unreadable doc behind 404 rather than 403, so an
	// anonymous probe cannot tell an existing private slug from a free one.
	rec := do(t, h, http.MethodGet, "/v1/docs/privdoc/versions/1/source", nil, "")
	if rec.Code != 404 {
		t.Fatalf("anonymous status = %d; want a hidden 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("document content leaked to an anonymous caller: %s", rec.Body.String())
	}
}

func TestVersionDiffReturnsBothLayers(t *testing.T) {
	h := newTestServer(t, nil)
	publishVersion(t, h, "diffdoc", "<html><body>\n<p>old</p>\n</body></html>")
	publishVersion(t, h, "diffdoc", "<html><body>\n<p>new</p>\n</body></html>")

	rec := do(t, h, http.MethodGet, "/v1/docs/diffdoc/diff?from=1&to=2", authorHdrNoCT(), "")
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	data := decodeDiffData(t, rec.Body.Bytes())
	if data["from"] != float64(1) || data["to"] != float64(2) {
		t.Fatalf("versions = %v/%v; want 1/2", data["from"], data["to"])
	}
	summary, _ := data["summary"].(map[string]any)
	if summary == nil || summary["modified"] == float64(0) {
		t.Fatalf("summary = %v; want a modified element", data["summary"])
	}
	changes, _ := data["changes"].([]any)
	if len(changes) == 0 {
		t.Fatalf("no structural changes: %s", rec.Body.String())
	}
	hunks, _ := data["code_hunks"].([]any)
	if len(hunks) == 0 {
		t.Fatalf("no code hunks: %s", rec.Body.String())
	}
	// The response describes the edit; it never echoes either document whole.
	if strings.Count(rec.Body.String(), "<html") > 2 {
		t.Fatalf("diff response looks like it carries full documents: %s", rec.Body.String())
	}
}

func TestVersionDiffRejectsBadQueries(t *testing.T) {
	h := newTestServer(t, nil)
	publishVersion(t, h, "qdoc", `<html><body><p>a</p></body></html>`)
	publishVersion(t, h, "qdoc", `<html><body><p>b</p></body></html>`)

	for _, query := range []string{
		"",                       // no params
		"?from=1",                // missing to
		"?to=2",                  // missing from
		"?from=1&to=2&from=3",    // repeated
		"?from=0&to=2",           // zero is not a published version
		"?from=latest&to=2",      // latest is not allowed in a diff pair
		"?from=1&to=2&unknown=1", // unexpected parameter
	} {
		rec := do(t, h, http.MethodGet, "/v1/docs/qdoc/diff"+query, authorHdrNoCT(), "")
		if rec.Code != 400 {
			t.Fatalf("query %q status = %d; want 400", query, rec.Code)
		}
	}
}

func TestVersionDiffMissingVersionIs404(t *testing.T) {
	h := newTestServer(t, nil)
	publishVersion(t, h, "missdoc", `<html><body><p>a</p></body></html>`)

	rec := do(t, h, http.MethodGet, "/v1/docs/missdoc/diff?from=1&to=9", authorHdrNoCT(), "")
	if rec.Code != 404 {
		t.Fatalf("status = %d; want 404", rec.Code)
	}
}

func TestVersionDiffRequiresReadAccess(t *testing.T) {
	h := newTestServer(t, nil)
	publishVersion(t, h, "privdiff", `<html><body><p>secret</p></body></html>`)
	publishVersion(t, h, "privdiff", `<html><body><p>secret2</p></body></html>`)

	rec := do(t, h, http.MethodGet, "/v1/docs/privdiff/diff?from=1&to=2", nil, "")
	if rec.Code != 404 {
		t.Fatalf("anonymous status = %d; want a hidden 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("document content leaked to an anonymous caller: %s", rec.Body.String())
	}
}

func TestVersionDiffOfSameVersionIsEmpty(t *testing.T) {
	h := newTestServer(t, nil)
	publishVersion(t, h, "samedoc", `<html><body><p>a</p></body></html>`)

	rec := do(t, h, http.MethodGet, "/v1/docs/samedoc/diff?from=1&to=1", authorHdrNoCT(), "")
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	data := decodeDiffData(t, rec.Body.Bytes())
	if changes, _ := data["changes"].([]any); len(changes) != 0 {
		t.Fatalf("a version compared with itself reported changes: %v", changes)
	}
	if hunks, _ := data["code_hunks"].([]any); len(hunks) != 0 {
		t.Fatalf("a version compared with itself reported hunks: %v", hunks)
	}
}
