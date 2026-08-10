package httpx_test

import (
	"fmt"
	"maps"
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
			slug := "publish-" + tc.role

			if rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody(slug, "owner-v1")); rec.Code != http.StatusOK {
				t.Fatalf("seed publish = %d: %s", rec.Code, rec.Body.String())
			}
			grant := fmt.Sprintf(`{"uid":"collaborator","role":%q}`, tc.role)
			if rec := do(t, h, http.MethodPut, "/v1/docs/"+slug+"/grants", authorHdr(), grant); rec.Code != http.StatusOK {
				t.Fatalf("grant %s = %d: %s", tc.role, rec.Code, rec.Body.String())
			}

			headers := map[string]string{octoUIDHeaderName: "collaborator", "Content-Type": "application/json"}
			rec := do(t, h, http.MethodPost, "/v1/docs", headers, publishBody(slug, "collaborator-v2"))
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s publish = %d; want %d: %s", tc.role, rec.Code, tc.wantStatus, rec.Body.String())
			}

			versions := do(t, h, http.MethodGet, "/v1/docs/"+slug+"/versions", authorHdrNoCT(), "")
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
	if rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody("publish-owned", "owner-v1")); rec.Code != http.StatusOK {
		t.Fatalf("seed publish = %d: %s", rec.Code, rec.Body.String())
	}

	headers := map[string]string{octoUIDHeaderName: "outsider", "Content-Type": "application/json"}
	rec := do(t, h, http.MethodPost, "/v1/docs", headers, publishBody("publish-owned", "outsider-v2"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("outsider publish = %d; want 404: %s", rec.Code, rec.Body.String())
	}

	versions := do(t, h, http.MethodGet, "/v1/docs/publish-owned/versions", authorHdrNoCT(), "")
	if strings.Contains(versions.Body.String(), `"n":2`) || !strings.Contains(versions.Body.String(), `"title":"owner-v1"`) {
		t.Fatalf("outsider publish mutated document: %s", versions.Body.String())
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

func TestUserPublishSpaceMembership(t *testing.T) {
	withStubIdentity(t, stubIdentity{uid: "new-author", spaces: map[string]bool{"space-1": true}})
	h := newTestServer(t, nil)
	base := map[string]string{octoUIDHeaderName: "new-author", "Content-Type": "application/json"}
	for _, tc := range []struct {
		name, space, token string
		want               int
	}{
		{"forged", "space-2", "user-token", http.StatusForbidden},
		{"missing token", "space-1", "", http.StatusForbidden},
		{"member", "space-1", "user-token", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := maps.Clone(base)
			if tc.token != "" {
				headers["token"] = tc.token
			}
			body := fmt.Sprintf(`{"slug":%q,"html":"<html><body>x</body></html>","space_id":%q}`, "space-"+strings.ReplaceAll(tc.name, " ", "-"), tc.space)
			rec := do(t, h, http.MethodPost, "/v1/docs", headers, body)
			if rec.Code != tc.want {
				t.Fatalf("publish = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestPublishExistingSlugAllowsAdmin(t *testing.T) {
	mirror := &stubMirror{
		slugToDoc: map[string]string{"publish-admin": "doc-admin"},
		roles:     map[string]int{"doc-admin|admin": service.DocMemberRoleAdmin},
	}
	h := newServerWithMirror(t, mirror)
	if rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody("publish-admin", "owner-v1")); rec.Code != http.StatusOK {
		t.Fatalf("seed publish = %d: %s", rec.Code, rec.Body.String())
	}

	headers := map[string]string{octoUIDHeaderName: "admin", "Content-Type": "application/json"}
	rec := do(t, h, http.MethodPost, "/v1/docs", headers, publishBody("publish-admin", "admin-v2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin republish = %d; want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestPublishExistingSlugAllowsCreator(t *testing.T) {
	h := newTestServer(t, nil)
	if rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody("publish-creator", "owner-v1")); rec.Code != http.StatusOK {
		t.Fatalf("seed publish = %d: %s", rec.Code, rec.Body.String())
	}
	rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody("publish-creator", "owner-v2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("creator republish = %d; want 200: %s", rec.Code, rec.Body.String())
	}
}
