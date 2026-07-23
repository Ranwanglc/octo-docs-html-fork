package httpx_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/log"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/transport/httpx"
)

// Plan③ A3 tests: bestCred picks CapAuthor via three ordered tiers —
// superAdmin → self-uid==creator → owner-admin in doc_member — before any
// reader path. Every test drives the real HTTP handler so the wiring
// (middleware → bestCred → gate) stays honest.

// stubMirror is a minimal service.DocMemberMirror for A3/A4/A6 tests. Only the
// reads bestCred/ListGrants touch are non-trivial; writes just record so
// caller-side assertions can inspect them.
type stubMirror struct {
	slugToDoc map[string]string
	// roles keyed by "docID|uid".
	roles       map[string]int
	listMembers map[string][]service.DocMember
	writes      []stubMirrorWrite
}

type stubMirrorWrite struct {
	op        string
	docID     string
	uid       string
	role      int
	grantedBy string
}

func (m *stubMirror) DocIDBySlug(_ context.Context, slug string) (string, bool, error) {
	if id, ok := m.slugToDoc[slug]; ok {
		return id, true, nil
	}
	return "", false, nil
}
func (m *stubMirror) UpsertDirectGrant(_ context.Context, docID, uid string, role int, grantedBy string) error {
	m.writes = append(m.writes, stubMirrorWrite{op: "upsert", docID: docID, uid: uid, role: role, grantedBy: grantedBy})
	if m.roles == nil {
		m.roles = map[string]int{}
	}
	m.roles[docID+"|"+uid] = role
	return nil
}
func (m *stubMirror) DeleteGrant(_ context.Context, docID, uid string) error {
	m.writes = append(m.writes, stubMirrorWrite{op: "delete", docID: docID, uid: uid})
	delete(m.roles, docID+"|"+uid)
	return nil
}
func (m *stubMirror) RoleByDocUID(_ context.Context, docID, uid string) (int, bool, error) {
	if r, ok := m.roles[docID+"|"+uid]; ok {
		return r, true, nil
	}
	return 0, false, nil
}
func (m *stubMirror) ListMembers(_ context.Context, docID string) ([]service.DocMember, error) {
	return m.listMembers[docID], nil
}

// newServerWithMirror builds the full HTTP server backed by an in-memory store
// AND a stubMirror pre-seeded by the caller — so A3② / A4 doc_member reads
// have a table to consult. Bot auth stays off so trust-header identity is the
// only auth path (keeps the fixture small).
func newServerWithMirror(t *testing.T, mirror *stubMirror) http.Handler {
	t.Helper()
	cfg := &config.Config{
		WriteToken: "test-token", MaxHTMLBytes: 5 << 20, RepoURL: "https://x",
		MaxAssetBytes:  25 << 20,
		AssetMIMEAllow: []string{"image/png"},
	}
	store := memory.New()
	locker := sluglock.NewMemory()
	comments := service.NewCommentService(store, locker)
	docs := service.NewDocService(store, store, comments, locker, cfg.BaseURL, cfg.MaxHTMLBytes)
	assets := service.NewAssetService(store, store, locker, cfg.MaxAssetBytes, cfg.AssetMIMEAllow)
	auth := service.NewAuthService(store, cfg, locker).WithDocMemberMirror(mirror)
	srv := httpx.New(httpx.Deps{
		Config: cfg, Logger: log.New("silent"), Docs: docs, Comments: comments,
		Assets: assets, Auth: auth, OverlayJS: "/* overlay */",
	})
	return srv.Handler()
}

// A3① (real user visiting own doc): creator_uid == selfUID → CapAuthor.
// Publish stamps testUID as creator; the same trust-header uid gets author.
func TestA3SelfUIDMatchesCreatorRealUser(t *testing.T) {
	mirror := &stubMirror{slugToDoc: map[string]string{"docA": "d1"}}
	h := newServerWithMirror(t, mirror)
	publish(t, h, "docA")
	rec := do(t, h, http.MethodPost, "/v1/docs/docA/share", authorHdrNoCT(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("A3① real-user own doc share = %d: %s", rec.Code, rec.Body.String())
	}
}

// A3② (owner visits bot-authored doc via doc_member admin):
// A real user reads a doc whose creator_uid is a different uid (bot),
// but doc_member has an admin (role=3) row for that user. Author must hold.
func TestA3OwnerAdminInDocMember(t *testing.T) {
	mirror := &stubMirror{
		slugToDoc: map[string]string{"docB": "d2"},
		roles:     map[string]int{"d2|owner-1": service.DocMemberRoleAdmin},
	}
	h := newServerWithMirror(t, mirror)
	// Seed the doc as testUID so creator_uid == testUID, NOT owner-1.
	publish(t, h, "docB")
	// owner-1 (trust-header, no bot session) tries an author-only op.
	rec := do(t, h, http.MethodPost, "/v1/docs/docB/share",
		map[string]string{octoUIDHeaderName: "owner-1"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("A3② owner-admin share = %d: %s", rec.Code, rec.Body.String())
	}
}

// A3① order guard: even when doc_member has an admin row for another uid,
// the caller’s own selfUID == creator match must fire first. Real user is
// creator; the mirror also lists an unrelated admin. Caller must be author
// via A3①, not silently promoted through A3② with someone else’s row.
func TestA3SelfUIDOrderBeforeDocMember(t *testing.T) {
	mirror := &stubMirror{
		slugToDoc: map[string]string{"docC": "d3"},
		// An unrelated admin exists; must not affect this caller’s decision.
		roles: map[string]int{"d3|someone-else": service.DocMemberRoleAdmin},
	}
	h := newServerWithMirror(t, mirror)
	publish(t, h, "docC") // creator_uid = testUID
	rec := do(t, h, http.MethodPost, "/v1/docs/docC/share", authorHdrNoCT(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("A3 order guard share = %d: %s", rec.Code, rec.Body.String())
	}
}

// A3 miss (stranger, no session, no doc_binding): three tiers all fall
// through → no cap → 404 on read gate. Confirms unrelated real users are
// still hidden when neither creator match nor doc_member admin nor doc_binding
// fires.
func TestA3AllTiersMissStrangerHidden(t *testing.T) {
	mirror := &stubMirror{slugToDoc: map[string]string{"docD": "d4"}}
	h := newServerWithMirror(t, mirror)
	publish(t, h, "docD")
	rec := do(t, h, http.MethodGet, "/d/docD/v/1",
		map[string]string{octoUIDHeaderName: "stranger"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stranger render = %d; want 404 (no cap)", rec.Code)
	}
}
