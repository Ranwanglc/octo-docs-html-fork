package httpx_test

import (
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
)

// Plan③ A4 tests: reader tier reads from doc_member (mirror wired) or falls
// back to meta.grants (mirror unwired). A4 must not shadow A3 — a doc_member
// admin row still authors via A3② and never lands as CapReader.

// A4 hit: mirror wired, selfUID has a reader row → CapReader.
// A stranger user reads a doc they only hold a forward-grant reader on; the
// author-only endpoint refuses them (403/404), but the reader HTML gate lets
// them through.
func TestA4ReaderRowLiftsCapReader(t *testing.T) {
	mirror := &stubMirror{
		slugToDoc: map[string]string{"docE": "d5"},
		roles:     map[string]int{"d5|reader-1": service.DocMemberRoleReader},
	}
	h := newServerWithMirror(t, mirror)
	publish(t, h, "docE")
	// reader-1 hits the HTML read gate — must succeed (200, HTML).
	rec := do(t, h, http.MethodGet, "/d/docE/v/1",
		map[string]string{octoUIDHeaderName: "reader-1"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("A4 reader HTML read = %d; want 200", rec.Code)
	}
	// Author endpoint must still refuse reader-1 (A4 does not smuggle author).
	rec = do(t, h, http.MethodPost, "/v1/docs/docE/share",
		map[string]string{octoUIDHeaderName: "reader-1"}, "")
	if rec.Code == http.StatusOK {
		t.Fatalf("A4 reader unexpectedly got author on share op")
	}
}

// A4 miss: doc_member has no row for the caller → CapReader path drops
// through; stranger stays hidden (404 on HTML gate).
func TestA4NoRowStrangerHidden(t *testing.T) {
	mirror := &stubMirror{slugToDoc: map[string]string{"docF": "d6"}}
	h := newServerWithMirror(t, mirror)
	publish(t, h, "docF")
	rec := do(t, h, http.MethodGet, "/d/docF/v/1",
		map[string]string{octoUIDHeaderName: "outsider"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("A4 stranger read = %d; want 404 (no cap)", rec.Code)
	}
}

// A3② outranks A4: a caller listed as admin (role=3) must land as CapAuthor,
// never merely CapReader. Order guard for the "reader path shadows author"
// regression the plan calls out explicitly.
func TestA4AdminRowStillAuthorsViaA3(t *testing.T) {
	mirror := &stubMirror{
		slugToDoc: map[string]string{"docG": "d7"},
		roles:     map[string]int{"d7|admin-1": service.DocMemberRoleAdmin},
	}
	h := newServerWithMirror(t, mirror)
	publish(t, h, "docG")
	// admin-1 tries an author-only op — must succeed via A3② (not A4).
	rec := do(t, h, http.MethodPost, "/v1/docs/docG/share",
		map[string]string{octoUIDHeaderName: "admin-1"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("A3②/A4 order: admin share = %d; want 200 (A3② wins)", rec.Code)
	}
}
