package httpx_test

import (
	"encoding/json"
	"net/http"
	"testing"
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
