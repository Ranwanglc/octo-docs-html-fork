package httpx_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
)

// createComment posts a comment as uid and returns its id.
func createComment(t *testing.T, h http.Handler, slug, uid string) string {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/v1/comments",
		map[string]string{octoUIDHeaderName: uid, "Content-Type": "application/json"},
		`{"slug":"`+slug+`","text":"note","version":1,"anchor":{"kind":"text","text":"hello"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create comment as %s = %d: %s", uid, rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Data.ID == "" {
		t.Fatalf("create comment body missing id: %s", rec.Body.String())
	}
	return created.Data.ID
}

//nolint:unparam // slug is a real parameter; kept explicit for readable call sites.
func reanchor(t *testing.T, h http.Handler, slug, id, uid string) int {
	t.Helper()
	rec := do(t, h, http.MethodPatch, "/v1/comments",
		map[string]string{octoUIDHeaderName: uid, "Content-Type": "application/json"},
		`{"slug":"`+slug+`","id":"`+id+`","anchor":{"kind":"text","text":"moved"},"version":1}`)
	return rec.Code
}

// PR #25 blocker #4: re-anchoring a comment is author-or-admin only. An ordinary
// writer (CapEdit) must NOT be able to move another author's anchor, even though
// writers retain the moderation escape on soft-delete. This locks the anchor to
// its author / CapManage.
func TestReanchorAuthorOnlyWriterCannotMoveOthersAnchor(t *testing.T) {
	slug := "reanchor-doc"
	key := service.DeriveDocKey(testUID, slug)
	mirror := &stubMirror{
		slugToDoc: map[string]string{key: "d-ra"},
		roles: map[string]int{
			// A commenter (the author) and an unrelated writer + admin.
			"d-ra|author-1": service.DocMemberRoleCommenter,
			"d-ra|writer-2": service.DocMemberRoleWriter,
			"d-ra|admin-3":  service.DocMemberRoleAdmin,
		},
	}
	h := newServerWithMirror(t, mirror)
	if k := publish(t, h, slug); k != key { // creator_uid = testUID (not any of the actors below)
		t.Fatalf("derived key mismatch: got %s want %s", k, key)
	}

	id := createComment(t, h, key, "author-1")

	// An ordinary writer (CapEdit) must be refused: cannot move another
	// author's anchor.
	if code := reanchor(t, h, key, id, "writer-2"); code != http.StatusForbidden {
		t.Fatalf("writer re-anchoring another author's comment = %d; want 403", code)
	}

	// A total stranger (no row) is hidden/refused too.
	if code := reanchor(t, h, key, id, "stranger"); code == http.StatusOK {
		t.Fatalf("stranger re-anchor unexpectedly allowed (got 200)")
	}

	// The comment's own author may re-anchor their own comment.
	if code := reanchor(t, h, key, id, "author-1"); code != http.StatusOK {
		t.Fatalf("author re-anchoring own comment = %d; want 200", code)
	}

	// An admin (CapManage) may re-anchor any comment (moderation reserved to
	// admin for the anchor-move op).
	if code := reanchor(t, h, key, id, "admin-3"); code != http.StatusOK {
		t.Fatalf("admin re-anchor = %d; want 200", code)
	}
}

// Companion: a superAdmin (owner) may re-anchor anything via the IsOwner
// short-circuit, unchanged by the tightened moderation tier.
func TestReanchorSuperAdminAllowed(t *testing.T) {
	slug := "reanchor-sa"
	key := service.DeriveDocKey(testUID, slug)
	mirror := &stubMirror{
		slugToDoc: map[string]string{key: "d-sa"},
		roles:     map[string]int{"d-sa|author-1": service.DocMemberRoleCommenter},
	}
	h := newServerWithMirror(t, mirror)
	publish(t, h, slug)
	id := createComment(t, h, key, "author-1")

	rec := do(t, h, http.MethodPatch, "/v1/comments",
		map[string]string{octoUIDHeaderName: "admin-uid", octoRoleHeaderName: "superAdmin", "Content-Type": "application/json"},
		`{"slug":"`+key+`","id":"`+id+`","anchor":{"kind":"text","text":"moved"},"version":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("superAdmin re-anchor = %d: %s", rec.Code, rec.Body.String())
	}
}

// A writer retains the moderation escape on soft-delete (unchanged behavior):
// this documents that only the anchor-move op was tightened, not delete.
func TestWriterMayModerateDeleteOthersComment(t *testing.T) {
	slug := "moddel-doc"
	key := service.DeriveDocKey(testUID, slug)
	mirror := &stubMirror{
		slugToDoc: map[string]string{key: "d-md"},
		roles: map[string]int{
			"d-md|author-1": service.DocMemberRoleCommenter,
			"d-md|writer-2": service.DocMemberRoleWriter,
		},
	}
	h := newServerWithMirror(t, mirror)
	publish(t, h, slug)
	id := createComment(t, h, key, "author-1")

	rec := do(t, h, http.MethodDelete, "/v1/comments?slug="+key+"&id="+id+"&version=1",
		map[string]string{octoUIDHeaderName: "writer-2"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("writer moderation delete = %d: %s (writers keep the delete escape)", rec.Code, rec.Body.String())
	}
}
