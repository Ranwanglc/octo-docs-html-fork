package httpx_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The `/v1/docs` owner index. Auth is a hand-rolled resolveViewerSession +
// IsOwner in the handler, so the tests exercise every branch through the real
// trust-header path (X-Octo-Role) rather than short-circuiting via the write
// token, which grants CapAuthor but does not build a session and would slip
// past IsOwner as "anonymous".

// ownerHeaders returns proxy headers that IsOwner accepts (Role=superAdmin).
func ownerHeaders() map[string]string {
	return map[string]string{
		"X-Octo-Uid":  "u-admin",
		"X-Octo-Name": "Admin",
		"X-Octo-Role": "superAdmin",
	}
}

// memberHeaders is a signed-in but non-owner viewer.
func memberHeaders() map[string]string {
	return map[string]string{
		"X-Octo-Uid":  "u-member",
		"X-Octo-Name": "Member",
		"X-Octo-Role": "member",
	}
}

func seedDoc(t *testing.T, h http.Handler, slug, title string) {
	t.Helper()
	auth := authorHdr()
	body := `{"slug":"` + slug + `","version":1,"html":"<html><body><h1>` + title + `</h1></body></html>","meta":{"title":"` + title + `"}}`
	rec := do(t, h, http.MethodPost, "/v1/docs", auth, body)
	if rec.Code != 200 {
		t.Fatalf("seed publish %s = %d: %s", slug, rec.Code, rec.Body.String())
	}
}

// parseListEnvelope decodes {"data":[...], "pagination":{...}} for assertions.
func parseListEnvelope(t *testing.T, body []byte) (data []map[string]any, pag map[string]any) {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal list envelope: %v: %s", err, string(body))
	}
	rawData, ok := env["data"]
	if !ok {
		t.Fatalf("missing data field: %s", string(body))
	}
	if rawData == nil {
		t.Fatalf("data must be [] not null: %s", string(body))
	}
	items, ok := rawData.([]any)
	if !ok {
		t.Fatalf("data not an array: %s", string(body))
	}
	for _, it := range items {
		m, _ := it.(map[string]any)
		data = append(data, m)
	}
	pag, _ = env["pagination"].(map[string]any)
	return
}

func parseDataEnvelope(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal data envelope: %v: %s", err, string(body))
	}
	data, _ := env["data"].(map[string]any)
	if data == nil {
		t.Fatalf("missing object data field: %s", string(body))
	}
	return data
}

func TestGetDocReturnsMetadata(t *testing.T) {
	h := newTestServer(t, nil)
	seedDoc(t, h, "detail", "Detail Title")
	auth := authorHdr()
	rec := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"detail","html":"<html><body><h1>Detail v2</h1></body></html>"}`)
	if rec.Code != 200 {
		t.Fatalf("publish v2 = %d: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/v1/docs/detail", authorHdrNoCT(), "")
	if rec.Code != 200 {
		t.Fatalf("get doc = %d: %s", rec.Code, rec.Body.String())
	}
	data := parseDataEnvelope(t, rec.Body.Bytes())
	if data["slug"] != "detail" || data["title"] != "Detail Title" {
		t.Fatalf("doc identity = %v", data)
	}
	if data["latest"] != float64(2) {
		t.Fatalf("latest = %v; want 2", data["latest"])
	}
	versions, _ := data["versions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("versions len = %d; want 2: %v", len(versions), data)
	}
	if updated, ok := data["updated"].(string); !ok || updated == "" {
		t.Fatalf("updated must be non-empty string: %v", data)
	}
}

func TestGetDocNotFound(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/v1/docs/missing", authorHdrNoCT(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing doc = %d; want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestGetDocUnauthenticated(t *testing.T) {
	h := newTestServer(t, nil)
	seedDoc(t, h, "private", "Private")
	rec := do(t, h, http.MethodGet, "/v1/docs/private", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unauthenticated doc = %d; want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestListDocsEmpty(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/v1/docs", ownerHeaders(), "")
	if rec.Code != 200 {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	// Empty store must emit "data":[], never "data":null (front-end contract).
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("empty store data must be []: %s", rec.Body.String())
	}
	data, pag := parseListEnvelope(t, rec.Body.Bytes())
	if len(data) != 0 {
		t.Fatalf("data len = %d; want 0", len(data))
	}
	if pag["total"].(float64) != 0 || pag["page"].(float64) != 1 || pag["page_size"].(float64) != 20 {
		t.Fatalf("default pagination = %v", pag)
	}
}

func TestListDocsDefaultPage(t *testing.T) {
	h := newTestServer(t, nil)
	for _, s := range []string{"a", "b", "c"} {
		seedDoc(t, h, s, strings.ToUpper(s))
	}
	rec := do(t, h, http.MethodGet, "/v1/docs", ownerHeaders(), "")
	if rec.Code != 200 {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	data, pag := parseListEnvelope(t, rec.Body.Bytes())
	if len(data) != 3 || pag["total"].(float64) != 3 {
		t.Fatalf("default page = %v, pag=%v", data, pag)
	}
	// Each item carries slug, title, latest. `updated` is the PR head feature:
	// seedDoc publishes a version, so DocService fills Versions[last].Created
	// with an RFC3339 timestamp — the field must materialise as a non-empty
	// string (nil / missing / empty would mean the wiring regressed).
	for _, it := range data {
		if it["slug"] == nil || it["title"] == nil || it["latest"] == nil {
			t.Fatalf("item missing required fields: %v", it)
		}
		upd, ok := it["updated"]
		if !ok {
			t.Fatalf("updated key missing after publish: %v", it)
		}
		s, isStr := upd.(string)
		if !isStr || s == "" {
			t.Fatalf("updated must be non-empty string: %v", it)
		}
	}
}

func TestListDocsPagedSlice(t *testing.T) {
	h := newTestServer(t, nil)
	for _, s := range []string{"a", "b", "c"} {
		seedDoc(t, h, s, s)
	}
	rec := do(t, h, http.MethodGet, "/v1/docs?page=2&page_size=1", ownerHeaders(), "")
	if rec.Code != 200 {
		t.Fatalf("paged = %d: %s", rec.Code, rec.Body.String())
	}
	data, pag := parseListEnvelope(t, rec.Body.Bytes())
	if len(data) != 1 || pag["total"].(float64) != 3 || pag["page"].(float64) != 2 || pag["page_size"].(float64) != 1 {
		t.Fatalf("page=2 slice = %v pag=%v", data, pag)
	}
}

func TestListDocsPageSizeClamp(t *testing.T) {
	h := newTestServer(t, nil)
	seedDoc(t, h, "one", "One")
	cases := []struct {
		q        string
		wantSize float64
		wantPage float64 // 0 = don't assert page (only size matters for this row)
	}{
		{"page_size=999", 100, 0},
		{"page_size=0", 20, 0},
		{"page_size=-5", 20, 0},
		{"page=0", 20, 1}, // page < 1 clamps to 1, page_size defaults to 20
	}
	for _, tc := range cases {
		rec := do(t, h, http.MethodGet, "/v1/docs?"+tc.q, ownerHeaders(), "")
		if rec.Code != 200 {
			t.Fatalf("%s = %d: %s", tc.q, rec.Code, rec.Body.String())
		}
		_, pag := parseListEnvelope(t, rec.Body.Bytes())
		if pag["page_size"].(float64) != tc.wantSize {
			t.Fatalf("%s → page_size=%v; want %v", tc.q, pag["page_size"], tc.wantSize)
		}
		if tc.wantPage != 0 && pag["page"].(float64) != tc.wantPage {
			t.Fatalf("%s → page=%v; want %v", tc.q, pag["page"], tc.wantPage)
		}
	}
}

func TestListDocsInvalidQuery(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/v1/docs?page=abc", ownerHeaders(), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric page = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"VALIDATION_ERROR"`) {
		t.Fatalf("expected VALIDATION_ERROR envelope: %s", rec.Body.String())
	}
}

func TestListDocsUnauthenticated(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/v1/docs", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session = %d; want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"AUTH_REQUIRED"`) {
		t.Fatalf("expected AUTH_REQUIRED envelope: %s", rec.Body.String())
	}
}

func TestListDocsForbiddenNonOwner(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/v1/docs", memberHeaders(), "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member = %d; want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"FORBIDDEN"`) {
		t.Fatalf("expected FORBIDDEN envelope: %s", rec.Body.String())
	}
}

func TestListDocsOutOfRangePage(t *testing.T) {
	h := newTestServer(t, nil)
	seedDoc(t, h, "only", "Only")
	rec := do(t, h, http.MethodGet, "/v1/docs?page=99", ownerHeaders(), "")
	if rec.Code != 200 {
		t.Fatalf("oob page = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("oob page data must be []: %s", rec.Body.String())
	}
	_, pag := parseListEnvelope(t, rec.Body.Bytes())
	if pag["total"].(float64) != 1 {
		t.Fatalf("oob total must still reflect real count: %v", pag)
	}
}

func TestListDocsLimitOffsetAlias(t *testing.T) {
	// limit/offset must map to page/page_size internally so external tooling that
	// speaks the alias syntax (older CLIs) gets the same slice as page/page_size.
	h := newTestServer(t, nil)
	for _, s := range []string{"a", "b", "c"} {
		seedDoc(t, h, s, s)
	}
	recA := do(t, h, http.MethodGet, "/v1/docs?limit=1&offset=1", ownerHeaders(), "")
	recB := do(t, h, http.MethodGet, "/v1/docs?page=2&page_size=1", ownerHeaders(), "")
	if recA.Code != 200 || recB.Code != 200 {
		t.Fatalf("alias=%d canonical=%d", recA.Code, recB.Code)
	}
	dataA, pagA := parseListEnvelope(t, recA.Body.Bytes())
	dataB, pagB := parseListEnvelope(t, recB.Body.Bytes())
	if len(dataA) != 1 || len(dataB) != 1 {
		t.Fatalf("alias len=%d canonical len=%d", len(dataA), len(dataB))
	}
	if dataA[0]["slug"] != dataB[0]["slug"] {
		t.Fatalf("alias slug=%v canonical slug=%v", dataA[0]["slug"], dataB[0]["slug"])
	}
	if pagA["page"] != pagB["page"] || pagA["page_size"] != pagB["page_size"] {
		t.Fatalf("alias pag=%v canonical pag=%v", pagA, pagB)
	}
}

// TestListDocsMixedInputCanonicalWins pins the MUST fix: when canonical `page`
// or `page_size` is present, junk `limit=xyz&offset=abc` in the same query
// must not trip validation. Response envelope must match the pure-canonical
// call byte-for-byte (data + pagination).
func TestListDocsMixedInputCanonicalWins(t *testing.T) {
	h := newTestServer(t, nil)
	for _, s := range []string{"a", "b", "c"} {
		seedDoc(t, h, s, s)
	}
	recMixed := do(t, h, http.MethodGet, "/v1/docs?page=2&page_size=1&offset=abc&limit=xyz", ownerHeaders(), "")
	recCanonical := do(t, h, http.MethodGet, "/v1/docs?page=2&page_size=1", ownerHeaders(), "")
	if recMixed.Code != 200 {
		t.Fatalf("mixed canonical+junk-alias = %d: %s", recMixed.Code, recMixed.Body.String())
	}
	if recCanonical.Code != 200 {
		t.Fatalf("canonical = %d: %s", recCanonical.Code, recCanonical.Body.String())
	}
	dataM, pagM := parseListEnvelope(t, recMixed.Body.Bytes())
	dataC, pagC := parseListEnvelope(t, recCanonical.Body.Bytes())
	if len(dataM) != len(dataC) {
		t.Fatalf("mixed len=%d canonical len=%d", len(dataM), len(dataC))
	}
	for i := range dataC {
		if dataM[i]["slug"] != dataC[i]["slug"] {
			t.Fatalf("row %d slug mismatch: mixed=%v canonical=%v", i, dataM[i]["slug"], dataC[i]["slug"])
		}
	}
	for _, k := range []string{"total", "page", "page_size"} {
		if pagM[k] != pagC[k] {
			t.Fatalf("pagination[%s] mixed=%v canonical=%v", k, pagM[k], pagC[k])
		}
	}
}

// TestListDocsOffsetAlignment locks the SHOULD-3 contract: alias offset that
// isn't a multiple of the resolved limit is rejected (400) rather than
// silently dropping the sub-page remainder into the next page.
func TestListDocsOffsetAlignment(t *testing.T) {
	h := newTestServer(t, nil)
	seedDoc(t, h, "only", "Only")
	rec := do(t, h, http.MethodGet, "/v1/docs?limit=20&offset=5", ownerHeaders(), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-aligned offset = %d; want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"VALIDATION_ERROR"`) {
		t.Fatalf("expected VALIDATION_ERROR envelope: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_offset_alignment") {
		t.Fatalf("expected invalid_offset_alignment reason: %s", rec.Body.String())
	}
}
