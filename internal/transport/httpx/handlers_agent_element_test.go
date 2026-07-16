package httpx_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// agentElementGet/Replace exercise the two per-aid agent endpoints end-to-end
// through the real server (write-token gated), covering: aid hit/miss, that a
// replace mints a new version whose HTML reflects the change, that a comment
// anchored to the doc survives the republish (reconcile ran), and that an
// out-of-bounds new_html (multi-element / script) is rejected.

// firstAID publishes html at slug v1 and returns the aid + tag of the first
// stamped artifact, by rendering v1 and scraping the stamped attribute.
func publishAndFirstAID(t *testing.T, h http.Handler, auth map[string]string, slug, html string) string {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/v1/docs", auth,
		`{"slug":"`+slug+`","html":`+jsonString(html)+`}`)
	if rec.Code != 200 {
		t.Fatalf("publish %s = %d: %s", slug, rec.Code, rec.Body.String())
	}
	// Render v1 and pull the first data-odoc-aid="..." value out of the HTML.
	body := do(t, h, http.MethodGet, "/d/"+slug+"/v/1", auth, "").Body.String()
	const marker = `data-odoc-aid="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no stamped aid in rendered %s: %s", slug, body)
	}
	rest := body[i+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatalf("unterminated aid attr in %s", slug)
	}
	return rest[:end]
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestAgentElementGetHitAndMiss(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	aid := publishAndFirstAID(t, h, auth,
		"elget", `<html><body><section><p>hello section</p></section></body></html>`)

	// Hit: returns the outer HTML of the stamped element.
	rec := do(t, h, http.MethodPost, "/v1/agent/element/get", auth,
		`{"slug":"elget","aid":"`+aid+`"}`)
	if rec.Code != 200 {
		t.Fatalf("element get = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Data struct {
			AID, Tag, HTML string
		}
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Data.AID != aid {
		t.Errorf("aid = %q, want %q", got.Data.AID, aid)
	}
	if got.Data.Tag != "section" {
		t.Errorf("tag = %q, want section", got.Data.Tag)
	}
	if !strings.Contains(got.Data.HTML, "hello section") || !strings.HasPrefix(got.Data.HTML, "<section") {
		t.Errorf("outer html unexpected: %q", got.Data.HTML)
	}

	// Miss: unknown aid → 404-style apperr.
	rec = do(t, h, http.MethodPost, "/v1/agent/element/get", auth,
		`{"slug":"elget","aid":"nope"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown aid get = %d; want 404", rec.Code)
	}
}

func TestAgentElementGetRequiresAuth(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPost, "/v1/agent/element/get",
		map[string]string{"Content-Type": "application/json"},
		`{"slug":"x","aid":"a"}`)
	// No identity resolves to less-than-author, which requireDocAuthorSlug hides
	// as 404 (existence + op both concealed) — not 401. Matches requireDocAuthor.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unauthenticated element get = %d; want 404", rec.Code)
	}
}

func TestAgentElementReplaceMakesNewVersion(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	aid := publishAndFirstAID(t, h, auth,
		"elrep", `<html><body><section><p>original text</p></section></body></html>`)

	// Anchor a comment to the doc so we can prove reconcile survives the republish.
	rec := do(t, h, http.MethodPost, "/v1/comments", auth,
		`{"slug":"elrep","text":"note","version":1,"anchor":{"kind":"text","text":"original"}}`)
	if rec.Code != 200 {
		t.Fatalf("seed comment = %d: %s", rec.Code, rec.Body.String())
	}

	// Replace the element (base_version omitted → latest).
	rec = do(t, h, http.MethodPost, "/v1/agent/element/replace", auth,
		`{"slug":"elrep","aid":"`+aid+`","new_html":`+jsonString(`<section><p>replaced text</p></section>`)+`}`)
	if rec.Code != 200 {
		t.Fatalf("element replace = %d: %s", rec.Code, rec.Body.String())
	}
	var pub struct {
		Data struct {
			Version int
		}
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pub)
	if pub.Data.Version != 2 {
		t.Fatalf("replace version = %d; want 2 (new version)", pub.Data.Version)
	}

	// v2 HTML reflects the change; the old content is gone.
	v2 := do(t, h, http.MethodGet, "/d/elrep/v/2", auth, "").Body.String()
	if !strings.Contains(v2, "replaced text") {
		t.Error("v2 missing the replaced content")
	}
	if strings.Contains(v2, "original text") {
		t.Error("v2 still shows the original content")
	}
	// v2 must be re-stamped (Publish stamped it, handler did not).
	if !strings.Contains(v2, "data-odoc-aid=") {
		t.Error("v2 not re-stamped")
	}

	// Reconcile ran: the seeded comment is still readable at v2.
	list := do(t, h, http.MethodGet, "/v1/comments?slug=elrep&version=2", auth, "").Body.String()
	if !strings.Contains(list, "note") {
		t.Errorf("comment lost after republish (reconcile did not run): %s", list)
	}
}

func TestAgentElementReplaceUnknownAID(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	_ = publishAndFirstAID(t, h, auth,
		"elbad", `<html><body><section><p>x</p></section></body></html>`)
	rec := do(t, h, http.MethodPost, "/v1/agent/element/replace", auth,
		`{"slug":"elbad","aid":"missing","new_html":`+jsonString(`<section><p>y</p></section>`)+`}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("replace unknown aid = %d; want 404", rec.Code)
	}
}

func TestAgentElementReplaceRejectsOutOfBounds(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	aid := publishAndFirstAID(t, h, auth,
		"eloob", `<html><body><section><p>x</p></section></body></html>`)

	cases := []struct {
		name    string
		newHTML string
	}{
		{"empty", ``},
		{"multi_element", `<section></section><section></section>`},
		{"script_fragment", `<script>alert(1)</script>`},
		{"plain_text", `just text`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, "/v1/agent/element/replace", auth,
				`{"slug":"eloob","aid":"`+aid+`","new_html":`+jsonString(c.newHTML)+`}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s new_html = %d; want 400: %s", c.name, rec.Code, rec.Body.String())
			}
		})
	}

	// A rejected replace must not have minted a new version.
	rec := do(t, h, http.MethodGet, "/v1/docs/eloob/versions", auth, "")
	if strings.Contains(rec.Body.String(), `"n":2`) {
		t.Errorf("out-of-bounds replace leaked a new version: %s", rec.Body.String())
	}
}

// Fix B/C/D at the API boundary: injection payloads that pass the naive
// top-level structural check (nested script, event handlers, javascript: URLs,
// non-void self-close) and stamper-owned data-odoc-* residue must all be
// rejected with 400 and must not mint a new version.
func TestAgentElementReplaceRejectsInjection(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	aid := publishAndFirstAID(t, h, auth,
		"elinj", `<html><body><section><p>x</p></section></body></html>`)

	cases := []struct {
		name    string
		newHTML string
	}{
		{"nested_script", `<section><div><script>alert(1)</script></div></section>`},
		{"void_onerror", `<img src=x onerror=alert(1)>`},
		{"toplevel_onload", `<section onload="x()"><p>y</p></section>`},
		{"javascript_url", `<section><a href="javascript:alert(1)">y</a></section>`},
		{"non_void_selfclose", `<section/>`},
		{"data_odoc_residue", `<section data-odoc-aid="forged"><p>y</p></section>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, "/v1/agent/element/replace", auth,
				`{"slug":"elinj","aid":"`+aid+`","new_html":`+jsonString(c.newHTML)+`}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s new_html = %d; want 400: %s", c.name, rec.Code, rec.Body.String())
			}
		})
	}
	// None of the rejected replaces may have minted v2.
	rec := do(t, h, http.MethodGet, "/v1/docs/elinj/versions", auth, "")
	if strings.Contains(rec.Body.String(), `"n":2`) {
		t.Errorf("injection replace leaked a new version: %s", rec.Body.String())
	}
}

// Fix E: a comment whose anchor is an ELEMENT anchor pointing at the replaced
// aid must be reconciled on republish — either rebound to the new aid or marked
// lost — never silently dropped. (The prior test only exercised a text anchor and
// only asserted the text was still readable, which does not prove element-anchor
// rebind/lost.)
func TestAgentElementReplaceReconcilesElementAnchor(t *testing.T) {
	h := newTestServer(t, nil)
	auth := authorHdr()
	aid := publishAndFirstAID(t, h, auth,
		"elanchor", `<html><body><section><p>original text</p></section></body></html>`)

	// Seed an ELEMENT-kind comment targeting the exact aid we will replace, with a
	// fingerprint so reconcile has a hint to rebind against.
	anchor := `{"kind":"element","aid":"` + aid + `","selector":"[data-odoc-aid=\"` + aid + `\"]","label":"section","fingerprint":{"tag":"section"}}`
	rec := do(t, h, http.MethodPost, "/v1/comments", auth,
		`{"slug":"elanchor","text":"element note","version":1,"anchor":`+anchor+`}`)
	if rec.Code != 200 {
		t.Fatalf("seed element comment = %d: %s", rec.Code, rec.Body.String())
	}

	// Replace the targeted element; Publish re-stamps (new aid) and reconciles.
	rec = do(t, h, http.MethodPost, "/v1/agent/element/replace", auth,
		`{"slug":"elanchor","aid":"`+aid+`","new_html":`+jsonString(`<section><p>replaced text</p></section>`)+`}`)
	if rec.Code != 200 {
		t.Fatalf("element replace = %d: %s", rec.Code, rec.Body.String())
	}

	// Read v2 comments: the seeded comment must still exist and its anchor must be
	// reconciled — rebound to a NEW element aid, or marked lost — never left stale.
	list := do(t, h, http.MethodGet, "/v1/comments?slug=elanchor&version=2", auth, "").Body.String()
	if !strings.Contains(list, "element note") {
		t.Fatalf("element-anchored comment lost after republish: %s", list)
	}
	var payload struct {
		Data []struct {
			Text   string `json:"text"`
			Anchor struct {
				Kind string `json:"kind"`
				AID  string `json:"aid"`
			} `json:"anchor"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(list), &payload); err != nil {
		t.Fatalf("decode comments v2: %v (%s)", err, list)
	}
	var found bool
	for _, c := range payload.Data {
		if c.Text != "element note" {
			continue
		}
		found = true
		switch c.Anchor.Kind {
		case "element":
			if c.Anchor.AID == "" {
				t.Errorf("rebound element anchor has empty aid: %s", list)
			}
			if c.Anchor.AID == aid {
				t.Errorf("anchor still points at the stale replaced aid %q; reconcile did not run", aid)
			}
		case "lost":
			// acceptable: reconcile ran but could not confidently rebind
		default:
			t.Errorf("unexpected anchor kind %q after reconcile: %s", c.Anchor.Kind, list)
		}
	}
	if !found {
		t.Fatalf("seeded element comment not found in v2: %s", list)
	}
}
