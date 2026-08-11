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
			slug := "publish-" + tc.role
			seedLegacyRef(t, h, slug, testUID)

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
	seedLegacyRef(t, h, "publish-owned", testUID)
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
	h := canonicalCreateServer(t, "publish-new", nil)
	headers := map[string]string{"Authorization": "Bearer publisher-token", "Content-Type": "application/json"}
	rec := do(t, h, http.MethodPost, "/v1/docs", headers, `{"idempotency_key":"publish-new-1","html":"<html><body><p>new-v1</p></body></html>"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first publish = %d; want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestPublishExistingSlugAllowsAdmin(t *testing.T) {
	mirror := &stubMirror{
		slugToDoc: map[string]string{"publish-admin": "doc-admin"},
		roles:     map[string]int{"doc-admin|admin": service.DocMemberRoleAdmin},
	}
	h := newServerWithMirror(t, mirror)
	seedLegacyRef(t, h, "publish-admin", testUID)
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
	seedLegacyRef(t, h, "publish-creator", testUID)
	if rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody("publish-creator", "owner-v1")); rec.Code != http.StatusOK {
		t.Fatalf("seed publish = %d: %s", rec.Code, rec.Body.String())
	}
	rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(), publishBody("publish-creator", "owner-v2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("creator republish = %d; want 200: %s", rec.Code, rec.Body.String())
	}
}
