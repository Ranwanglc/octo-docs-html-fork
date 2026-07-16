package httpx_test

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// Asset sub-resource signing: a rendered doc's inline /d/{slug}/assets/{sha}
// URLs come back stamped with ?sig=&exp=, and that signed URL serves the bytes
// with NO other credential (the path a browser's native <img> load takes, which
// can carry no token header). Tampered/absent signatures fall back to the reader
// gate (404 uncredentialed).

var signedAssetRe = regexp.MustCompile(`/d/pics/assets/([0-9a-f]{64})\?sig=([^"&]+)&exp=([0-9]+)`)

func seedDocWithAsset(t *testing.T, h http.Handler) string {
	t.Helper()
	// Upload an asset first (author).
	body, ct := multipartFile(t, "cat.gif", gifBytes)
	rec := do(t, h, http.MethodPost, "/v1/docs/pics/assets",
		map[string]string{octoUIDHeaderName: testUID, "Content-Type": ct}, body)
	// The doc must exist to hang the asset off — publish it referencing the asset.
	// Upload requires the doc; publish first if the upload 404'd.
	if rec.Code == http.StatusNotFound {
		if pr := do(t, h, http.MethodPost, "/v1/docs", authorHdr(),
			`{"slug":"pics","html":"<html><body><p>x</p></body></html>"}`); pr.Code != 200 {
			t.Fatalf("seed publish = %d: %s", pr.Code, pr.Body.String())
		}
		body, ct = multipartFile(t, "cat.gif", gifBytes)
		rec = do(t, h, http.MethodPost, "/v1/docs/pics/assets",
			map[string]string{octoUIDHeaderName: testUID, "Content-Type": ct}, body)
	}
	if rec.Code != 200 {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body.String())
	}
	var up map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &up)
	upData, _ := up["data"].(map[string]any)
	sha, _ := upData["sha256"].(string)
	if len(sha) != 64 {
		t.Fatalf("upload sha = %q", sha)
	}
	// (Re)publish the doc with an <img> referencing the asset.
	html := `<html><body><img src="/d/pics/assets/` + sha + `"></body></html>`
	if pr := do(t, h, http.MethodPost, "/v1/docs",
		authorHdr(), `{"slug":"pics","html":`+jsonString(html)+`}`); pr.Code != 200 {
		t.Fatalf("republish = %d: %s", pr.Code, pr.Body.String())
	}
	return sha
}

func TestRenderSignsAssetURLsAndServes(t *testing.T) {
	h := newTestServer(t, nil)
	sha := seedDocWithAsset(t, h)

	// Render as the author (creator trust header). Rendered HTML must carry the
	// asset URL stamped with a signature.
	rec := do(t, h, http.MethodGet, "/d/pics/v/latest",
		map[string]string{octoUIDHeaderName: testUID}, "")
	if rec.Code != 200 {
		t.Fatalf("render = %d: %s", rec.Code, rec.Body.String())
	}
	m := signedAssetRe.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("rendered HTML has no signed asset URL; body:\n%s", rec.Body.String())
	}
	if m[1] != sha {
		t.Fatalf("signed sha = %s; want %s", m[1], sha)
	}
	signedPath := "/d/pics/assets/" + sha + "?sig=" + m[2] + "&exp=" + m[3]

	// The signed URL serves the bytes with NO credential (browser <img> path).
	rec = do(t, h, http.MethodGet, signedPath, nil, "")
	if rec.Code != 200 {
		t.Fatalf("signed serve = %d; want 200 (no cred needed): %s", rec.Code, rec.Body.String())
	}

	// Unsigned + uncredentialed still 404 (existence hidden).
	if rec := do(t, h, http.MethodGet, "/d/pics/assets/"+sha, nil, ""); rec.Code != 404 {
		t.Fatalf("unsigned uncredentialed = %d; want 404", rec.Code)
	}

	// Tampered signature falls back to reader gate → 404.
	tampered := "/d/pics/assets/" + sha + "?sig=" + strings.Repeat("A", len(m[2])) + "&exp=" + m[3]
	if rec := do(t, h, http.MethodGet, tampered, nil, ""); rec.Code != 404 {
		t.Fatalf("tampered sig = %d; want 404", rec.Code)
	}

	// Expired signature (exp in the past) rejected → 404.
	expired := "/d/pics/assets/" + sha + "?sig=" + m[2] + "&exp=1"
	if rec := do(t, h, http.MethodGet, expired, nil, ""); rec.Code != 404 {
		t.Fatalf("expired sig = %d; want 404", rec.Code)
	}
}
