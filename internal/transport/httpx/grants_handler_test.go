package httpx_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
)

// doc_grants: an author can list/grant/revoke per-uid access; a granted uid
// resolves to reader (can read a private doc), a non-granted uid stays 404, and
// the creator is never demoted by a grant. Exercises the full HTTP path against
// the trust-header identity used by the rest of the suite.

// grantAsOwner PUTs a reader grant for uid, acting as the doc's owner (author).
func grantAsOwner(t *testing.T, h http.Handler, slug, owner, uid string) *http.Response {
	t.Helper()
	rec := do(t, h, http.MethodPut, "/v1/docs/"+slug+"/grants",
		map[string]string{octoUIDHeaderName: owner, "Content-Type": "application/json"},
		`{"uid":"`+uid+`","role":"reader"}`)
	return rec.Result()
}

func TestGrantGivesReaderAndRevokeRemovesIt(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	h := newTestServer(t, ownerAuthCfg())
	publishAsBot(t, h, "docG")

	// Before any grant, an unrelated user cannot read the private doc → 404.
	rec := do(t, h, http.MethodGet, "/v1/docs/docG/versions",
		map[string]string{octoUIDHeaderName: "friend-1"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("pre-grant read = %d; want 404: %s", rec.Code, rec.Body.String())
	}

	// Owner grants friend-1 reader.
	if resp := grantAsOwner(t, h, "docG", "owner-1", "friend-1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("grant = %d; want 200", resp.StatusCode)
	}

	// Now friend-1 can read (reader).
	rec = do(t, h, http.MethodGet, "/v1/docs/docG/versions",
		map[string]string{octoUIDHeaderName: "friend-1"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("post-grant read = %d; want 200: %s", rec.Code, rec.Body.String())
	}

	// But reader is not author: friend-1 cannot delete → 404 (hides the op).
	rec = do(t, h, http.MethodDelete, "/v1/docs/docG",
		map[string]string{octoUIDHeaderName: "friend-1"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reader delete = %d; want 404: %s", rec.Code, rec.Body.String())
	}

	// Revoke → friend-1 back to 404.
	rec = do(t, h, http.MethodDelete, "/v1/docs/docG/grants/friend-1",
		map[string]string{octoUIDHeaderName: "owner-1"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d; want 200: %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/v1/docs/docG/versions",
		map[string]string{octoUIDHeaderName: "friend-1"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("post-revoke read = %d; want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestListGrantsShowsCreatorAndMembers(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	h := newTestServer(t, ownerAuthCfg())
	publishAsBot(t, h, "docL")
	grantAsOwner(t, h, "docL", "owner-1", "friend-1")

	rec := do(t, h, http.MethodGet, "/v1/docs/docL/grants",
		map[string]string{octoUIDHeaderName: "owner-1"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list grants = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Items []struct {
				UID    string `json:"uid"`
				Role   string `json:"role"`
				Source string `json:"source"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	var sawOwner, sawFriend bool
	for _, it := range body.Data.Items {
		if it.UID == "owner-1" && it.Source == "owner" {
			sawOwner = true
		}
		if it.UID == "friend-1" && it.Role == "reader" && it.Source == "direct" {
			sawFriend = true
		}
	}
	if !sawOwner || !sawFriend {
		t.Fatalf("list missing rows: owner=%v friend=%v (%s)", sawOwner, sawFriend, rec.Body.String())
	}
}

func TestCreatorCannotBeRemovedAsGrant(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	h := newTestServer(t, ownerAuthCfg())
	publishAsBot(t, h, "docC")

	rec := do(t, h, http.MethodDelete, "/v1/docs/docC/grants/owner-1",
		map[string]string{octoUIDHeaderName: "owner-1"}, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("remove creator = %d; want 409: %s", rec.Code, rec.Body.String())
	}
}

func TestNonAuthorCannotManageGrants(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	h := newTestServer(t, ownerAuthCfg())
	publishAsBot(t, h, "docN")

	// A non-author uid hitting the author-only grants routes must see 404.
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/docs/docN/grants"},
		{http.MethodPut, "/v1/docs/docN/grants"},
		{http.MethodDelete, "/v1/docs/docN/grants/x"},
	} {
		rec := do(t, h, tc.method, tc.path,
			map[string]string{octoUIDHeaderName: "stranger", "Content-Type": "application/json"},
			`{"uid":"x","role":"reader"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s by non-author = %d; want 404: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// P2-A: revoking a doc_member admin (not the creator) previously returned
// HTTP 500 because ErrGrantProtected was a bare errors.New sentinel and
// writeErr's errors.As(&apperr.Error) fell through to the generic 500 branch.
// After P2-A the sentinel is apperr.Conflict so the response is 409.
func TestRemoveAdminGrantReturns409(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	// Wire the mirror with an admin row on a non-creator uid so the pre-check
	// (creator == uid) misses and the request reaches RemoveGrant, where the
	// admin-protection sentinel fires. Publish stamps owner-1 as creator, so
	// we protect a separate admin uid ("admin-uid").
	mirror := &stubMirror{
		slugToDoc: map[string]string{"docP2A": "dP2A"},
		roles:     map[string]int{"dP2A|admin-uid": 3},
	}
	h, _ := newServerWithMirrorAndBotAuth(t, mirror)
	publishAsBot(t, h, "docP2A")

	rec := do(t, h, http.MethodDelete, "/v1/docs/docP2A/grants/admin-uid",
		map[string]string{octoUIDHeaderName: "owner-1"}, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("revoke admin = %d; want 409 (P2-A apperr.Conflict): %s", rec.Code, rec.Body.String())
	}
}

// P2-A: granting reader to a doc_member admin returns HTTP 409 as well,
// via the same ErrGrantProtected sentinel from P1-B's downgrade guard.
func TestAddGrantDowngradeAdminReturns409(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	mirror := &stubMirror{
		slugToDoc: map[string]string{"docP2AA": "dP2AA"},
		roles:     map[string]int{"dP2AA|admin-uid": 3},
	}
	h, _ := newServerWithMirrorAndBotAuth(t, mirror)
	publishAsBot(t, h, "docP2AA")

	rec := do(t, h, http.MethodPut, "/v1/docs/docP2AA/grants",
		map[string]string{octoUIDHeaderName: "owner-1", "Content-Type": "application/json"},
		`{"uid":"admin-uid","role":"reader"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("grant downgrade admin = %d; want 409: %s", rec.Code, rec.Body.String())
	}
}

// P2-B: on the wired path, GET /v1/docs/{slug}/grants must return the creator
// exactly once — as the handler-synthesised {role:"author", source:"owner"}
// row. If the service surfaces the creator from doc_member as well, the UI
// renders the creator twice (once as author, once as a deletable direct
// grant). Repro: mirror wired, doc_member has the creator admin row (M1
// backfill) + a real reader friend.
func TestListGrantsWiredNoCreatorDup(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	mirror := &stubMirror{
		slugToDoc: map[string]string{"docLD": "dLD"},
		listMembers: map[string][]service.DocMember{
			"dLD": {
				{UID: "owner-1", Role: 3, GrantedBy: "system"},
				{UID: "friend-1", Role: 1, GrantedBy: "owner-1"},
			},
		},
	}
	h, _ := newServerWithMirrorAndBotAuth(t, mirror)
	publishAsBot(t, h, "docLD")

	rec := do(t, h, http.MethodGet, "/v1/docs/docLD/grants",
		map[string]string{octoUIDHeaderName: "owner-1"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list grants = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Items []struct {
				UID    string `json:"uid"`
				Role   string `json:"role"`
				Source string `json:"source"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	creatorRows := 0
	for _, it := range body.Data.Items {
		if it.UID == "owner-1" {
			creatorRows++
			if it.Role != "author" || it.Source != "owner" {
				t.Fatalf("creator row = %+v; want role=author source=owner", it)
			}
		}
	}
	if creatorRows != 1 {
		t.Fatalf("creator rows = %d; want exactly 1 (no doc_member duplication)", creatorRows)
	}
	// Sanity: friend row still comes through as a direct reader.
	friend := false
	for _, it := range body.Data.Items {
		if it.UID == "friend-1" && it.Role == "reader" && it.Source == "direct" {
			friend = true
		}
	}
	if !friend {
		t.Fatalf("friend reader row missing: %+v", body.Data.Items)
	}
}
