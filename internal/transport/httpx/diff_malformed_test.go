package httpx_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestVersionDiffHandlesMalformedHTMLEndToEnd is the published-version HTTP
// regression for the malformed-diff hardening (PR #25 blocker #1). Two published
// versions carry an unclosed '<' tail; the diff endpoint must respond cleanly
// (200 or a fail-closed error status) rather than panic / 500-with-stack. Before
// the fix, normalizedHTMLLines sliced an empty tag range and panicked.
func TestVersionDiffHandlesMalformedHTMLEndToEnd(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	// Both payloads end in a dangling '<' (JSON-escaped) so the stored source
	// has an unclosed tag tail; they differ so a diff is actually computed.
	payloads := []string{
		`{"slug":"mdiff","html":"<html><body><p>alpha</p>abc<"}`,
		`{"slug":"mdiff","html":"<html><body><p>beta</p>xyz<"}`,
	}
	for _, payload := range payloads {
		rec := do(t, h, http.MethodPost, "/v1/docs", auth, payload)
		if rec.Code != http.StatusOK {
			t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
		}
	}
	rec := do(t, h, http.MethodGet, "/v1/docs/mdiff/diff?from=1&to=2", authorHdrNoCT(), "")
	// Must not panic / crash. Accept OK (diffed as text) or a controlled 4xx/5xx
	// envelope, but never an empty/garbled response.
	if rec.Code == 0 {
		t.Fatalf("diff produced no response (handler crashed?)")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("malformed diff = %d: %s (want 200; parser should treat tail as text)", rec.Code, rec.Body.String())
	}
}

func TestVersionDiffAcceptsEOFRecoveredCommentEndToEnd(t *testing.T) {
	h := newTestServer(t, nil)
	for _, source := range []string{
		`<html><body><p>old</p><!-- draft`,
		`<html><body><p>new</p><!-- draft`,
	} {
		payload, err := json.Marshal(map[string]string{"slug": "open-comment", "html": source})
		if err != nil {
			t.Fatal(err)
		}
		rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), string(payload))
		if rec.Code != http.StatusOK {
			t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
		}
	}
	rec := do(t, h, http.MethodGet, "/v1/docs/open-comment/diff?from=1&to=2", authorHdrNoCT(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("EOF-recovered comment diff = %d: %s", rec.Code, rec.Body.String())
	}
}
