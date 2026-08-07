package httpx_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
)

func publishBody(slug, title string) string {
	return fmt.Sprintf(`{"slug":%q,"html":"<html><body><p>%s</p></body></html>","title":%q}`, slug, title, title)
}

func TestPublishExistingSlugRequiresEditCapability(t *testing.T) {
	for _, tc := range []struct {
		name       string
		role       string
		wantStatus int
	}{
		{name: "reader rejected", role: "reader", wantStatus: http.StatusNotFound},
		{name: "commenter rejected", role: "commenter", wantStatus: http.StatusNotFound},
		{name: "writer allowed", role: "writer", wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestServer(t, nil)
			alias := "publish-" + tc.role

			rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody(alias, "owner-v1"))
			if rec.Code != http.StatusOK {
				t.Fatalf("seed publish = %d: %s", rec.Code, rec.Body.String())
			}
			// The doc lives under the server-derived key; grants and republishes must
			// address that key, not the alias (a foreign alias derives a different key).
			key := pubKey(t, rec)
			grant := fmt.Sprintf(`{"uid":"collaborator","role":%q}`, tc.role)
			if rec := do(t, h, http.MethodPut, "/v1/docs/"+key+"/grants", authorHdr(), grant); rec.Code != http.StatusOK {
				t.Fatalf("grant %s = %d: %s", tc.role, rec.Code, rec.Body.String())
			}

			headers := map[string]string{octoUIDHeaderName: "collaborator", "Content-Type": "application/json"}
			rec = do(t, h, http.MethodPost, "/v1/docs", headers, publishBody(key, "collaborator-v2"))
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s publish = %d; want %d: %s", tc.role, rec.Code, tc.wantStatus, rec.Body.String())
			}

			versions := do(t, h, http.MethodGet, "/v1/docs/"+key+"/versions", authorHdrNoCT(), "")
			if tc.wantStatus == http.StatusOK {
				if !strings.Contains(versions.Body.String(), `"n":2`) {
					t.Fatalf("editor publish did not create v2: %s", versions.Body.String())
				}
			} else if strings.Contains(versions.Body.String(), `"n":2`) || !strings.Contains(versions.Body.String(), `"title":"owner-v1"`) {
				t.Fatalf("rejected publish mutated document: %s", versions.Body.String())
			}
		})
	}
}

func TestPublishExistingSlugRejectsUnrelatedIdentity(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody("publish-owned", "owner-v1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed publish = %d: %s", rec.Code, rec.Body.String())
	}
	key := pubKey(t, rec)

	// Addressing the owner's document directly by its key must be refused: an
	// unrelated identity has no edit capability on it.
	headers := map[string]string{octoUIDHeaderName: "outsider", "Content-Type": "application/json"}
	rec = do(t, h, http.MethodPost, "/v1/docs", headers, publishBody(key, "outsider-v2"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("outsider publish = %d; want 404: %s", rec.Code, rec.Body.String())
	}

	versions := do(t, h, http.MethodGet, "/v1/docs/"+key+"/versions", authorHdrNoCT(), "")
	if strings.Contains(versions.Body.String(), `"n":2`) || !strings.Contains(versions.Body.String(), `"title":"owner-v1"`) {
		t.Fatalf("outsider publish mutated document: %s", versions.Body.String())
	}
}

// TestPublishSameAliasDifferentIdentityIsolated pins the namespacing property: a
// second identity publishing under the SAME human alias gets its own document
// under its own derived key, and the first identity's document is untouched. This
// is what removes global alias squatting — the outsider never needs to be refused
// because it can never address someone else's doc by name.
func TestPublishSameAliasDifferentIdentityIsolated(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody("weekly-report", "owner-v1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed publish = %d: %s", rec.Code, rec.Body.String())
	}
	ownerKey := pubKey(t, rec)

	headers := map[string]string{octoUIDHeaderName: "other-author", "Content-Type": "application/json"}
	rec = do(t, h, http.MethodPost, "/v1/docs", headers, publishBody("weekly-report", "other-v1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("second identity publish = %d; want 200: %s", rec.Code, rec.Body.String())
	}
	otherKey := pubKey(t, rec)
	if otherKey == ownerKey {
		t.Fatalf("same alias from a different identity reused key %q", ownerKey)
	}

	// The first document still has exactly one version and its original title.
	versions := do(t, h, http.MethodGet, "/v1/docs/"+ownerKey+"/versions", authorHdrNoCT(), "")
	if strings.Contains(versions.Body.String(), `"n":2`) || !strings.Contains(versions.Body.String(), `"title":"owner-v1"`) {
		t.Fatalf("foreign publish mutated the owner document: %s", versions.Body.String())
	}
}

func TestPublishNewSlugAllowsAuthenticatedIdentity(t *testing.T) {
	h := newTestServer(t, nil)
	headers := map[string]string{octoUIDHeaderName: "new-author", "Content-Type": "application/json"}
	rec := do(t, h, http.MethodPost, "/v1/docs", headers, publishBody("publish-new", "new-v1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("first publish = %d; want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestPublishExistingSlugAllowsAdmin(t *testing.T) {
	// The mirror is keyed by the doc's storage key, which is derived — so seed the
	// doc first, then wire the mirror entry for the resolved key.
	mirror := &stubMirror{
		slugToDoc: map[string]string{},
		roles:     map[string]int{"doc-admin|admin": service.DocMemberRoleAdmin},
	}
	h := newServerWithMirror(t, mirror)
	rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody("publish-admin", "owner-v1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed publish = %d: %s", rec.Code, rec.Body.String())
	}
	key := pubKey(t, rec)
	mirror.slugToDoc[key] = "doc-admin"

	headers := map[string]string{octoUIDHeaderName: "admin", "Content-Type": "application/json"}
	rec = do(t, h, http.MethodPost, "/v1/docs", headers, publishBody(key, "admin-v2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin republish = %d; want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestPublishExistingSlugAllowsCreator(t *testing.T) {
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody("publish-creator", "owner-v1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed publish = %d: %s", rec.Code, rec.Body.String())
	}
	key := pubKey(t, rec)
	// Both addressing forms must reach the same document: by alias (the creator's
	// own alias re-derives the same key) and by key.
	rec = do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody("publish-creator", "owner-v2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("creator republish by alias = %d; want 200: %s", rec.Code, rec.Body.String())
	}
	if got := pubKey(t, rec); got != key {
		t.Fatalf("creator republish by alias landed on %q; want %q", got, key)
	}
	rec = do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody(key, "owner-v3"))
	if rec.Code != http.StatusOK {
		t.Fatalf("creator republish by key = %d; want 200: %s", rec.Code, rec.Body.String())
	}
}
