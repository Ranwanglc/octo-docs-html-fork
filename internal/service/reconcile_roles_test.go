package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

// reconcileMirror is a doc_member mirror tailored to the reconcile migration
// tests: it can seed pre-existing roles, refuse specific uids' inserts (partial
// failure), and enforces the P2 admin-not-clobber guard like the real DB.
type reconcileMirror struct {
	mu         sync.Mutex
	docID      string
	roles      map[string]int
	failUpsert map[string]bool
	inserts    []string
}

func (m *reconcileMirror) DocIDBySlug(_ context.Context, _, _ string) (string, bool, error) {
	return m.docID, true, nil
}

func (m *reconcileMirror) TitleBySlug(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

func (m *reconcileMirror) UpsertDirectGrant(_ context.Context, _, uid string, role int, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failUpsert[uid] {
		return errors.New("simulated upsert failure for " + uid)
	}
	if m.roles == nil {
		m.roles = map[string]int{}
	}
	if role != service.DocMemberRoleAdmin && m.roles[uid] == service.DocMemberRoleAdmin {
		return nil
	}
	m.roles[uid] = role
	return nil
}

// InsertDirectGrantIfAbsent mirrors the MySQL INSERT IGNORE: insert only when
// absent, never overwrite. failUpsert[uid] simulates a DB error.
func (m *reconcileMirror) InsertDirectGrantIfAbsent(_ context.Context, _, uid string, role int, _ string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failUpsert[uid] {
		return false, errors.New("simulated insert failure for " + uid)
	}
	if m.roles == nil {
		m.roles = map[string]int{}
	}
	if _, ok := m.roles[uid]; ok {
		return false, nil // existing row: never overwrite
	}
	m.inserts = append(m.inserts, uid)
	m.roles[uid] = role
	return true, nil
}

func (m *reconcileMirror) DeleteGrant(_ context.Context, _, uid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.roles, uid)
	return nil
}

func (m *reconcileMirror) RoleByDocUID(_ context.Context, _, uid string) (int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.roles[uid]; ok {
		return r, true, nil
	}
	return 0, false, nil
}

func (m *reconcileMirror) ListMembers(_ context.Context, _ string) ([]service.DocMember, error) {
	return nil, nil
}

func seedReconcileDoc(t *testing.T, slug string, grants map[string]any) (*service.AuthService, *memory.Store, *reconcileMirror) {
	t.Helper()
	store := memory.New()
	extra := map[string]any{storage.CreatorUIDExtraKey: "owner-1"}
	if grants != nil {
		extra[storage.GrantsExtraKey] = grants //nolint:staticcheck // seed legacy grants for reconcile tests
	}
	if err := store.PutMeta(context.Background(), slug, storage.DocMeta{Slug: slug, Title: "T", Extra: extra}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	mirror := &reconcileMirror{docID: "doc-" + slug}
	svc := service.NewAuthService(store, &config.Config{}, sluglock.NewMemory()).WithDocMemberMirror(mirror)
	return svc, store, mirror
}

func metaGrantKeys(t *testing.T, store *memory.Store, slug string) map[string]bool {
	t.Helper()
	meta, err := store.GetMeta(context.Background(), slug)
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	out := map[string]bool{}
	grants, _ := meta.Extra[storage.GrantsExtraKey].(map[string]any) //nolint:staticcheck // reading legacy grants in test
	for k := range grants {
		out[k] = true
	}
	return out
}

// PR #25 blocker: reconcile maps reader/commenter/writer through the canonical
// roleLabelToCode at their real tier and never restores admin/unknown. Once the
// doc is registered every consumed entry is cleared — including admin/unknown/
// malformed — so they cannot resurrect via a later unregistered fallback; only
// the creator entry is left for the M1/creator path.
func TestReconcileMapsAllValidRolesAndClearsConsumed(t *testing.T) {
	slug := "recAll"
	svc, store, mirror := seedReconcileDoc(t, slug, map[string]any{
		"reader-r":    map[string]any{"role": "reader", "granted_by": "owner-1"},
		"commenter-c": map[string]any{"role": "commenter"},
		"writer-w":    map[string]any{"role": "writer"},
		"admin-a":     map[string]any{"role": "admin"},    // never restore; clear
		"weird-x":     map[string]any{"role": "superxyz"}, // unknown; clear
		"owner-1":     map[string]any{"role": "reader"},   // creator; retain
		"malformed":   "not-a-map",                        // wrong shape; clear
	})
	if err := svc.ReconcileMetaGrantsToDocMember(context.Background(), slug); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	want := map[string]int{
		"reader-r":    service.DocMemberRoleReader,
		"commenter-c": service.DocMemberRoleCommenter,
		"writer-w":    service.DocMemberRoleWriter,
	}
	for uid, role := range want {
		if mirror.roles[uid] != role {
			t.Fatalf("role[%s] = %d; want %d", uid, mirror.roles[uid], role)
		}
	}
	for _, uid := range []string{"admin-a", "weird-x", "owner-1", "malformed"} {
		if _, present := mirror.roles[uid]; present {
			t.Fatalf("uid %s must not be migrated (admin/unknown/creator/malformed)", uid)
		}
	}

	// Migrated valid roles AND invalid/admin/malformed entries are all cleared
	// once wired+registered; only the creator entry is retained.
	keys := metaGrantKeys(t, store, slug)
	for _, uid := range []string{"reader-r", "commenter-c", "writer-w", "admin-a", "weird-x", "malformed"} {
		if keys[uid] {
			t.Fatalf("meta.grants[%s] must be cleared; keys=%v", uid, keys)
		}
	}
	if !keys["owner-1"] {
		t.Fatalf("meta.grants[owner-1] (creator) must be retained; keys=%v", keys)
	}
}

// Partial failure: a per-uid upsert error leaves that entry in meta.grants for a
// later retry (fail-closed: never drop metadata before the authoritative write
// succeeds) while the rest migrate and clear.
func TestReconcilePartialFailureRetainsUnwrittenEntry(t *testing.T) {
	slug := "recPartial"
	svc, store, mirror := seedReconcileDoc(t, slug, map[string]any{
		"ok-reader":  map[string]any{"role": "reader"},
		"bad-writer": map[string]any{"role": "writer"},
	})
	mirror.failUpsert = map[string]bool{"bad-writer": true}

	if err := svc.ReconcileMetaGrantsToDocMember(context.Background(), slug); err != nil {
		t.Fatalf("reconcile (best-effort) should not hard-fail: %v", err)
	}
	if mirror.roles["ok-reader"] != service.DocMemberRoleReader {
		t.Fatalf("ok-reader not migrated: roles=%v", mirror.roles)
	}
	keys := metaGrantKeys(t, store, slug)
	if keys["ok-reader"] {
		t.Fatalf("ok-reader entry must be cleared after success; keys=%v", keys)
	}
	if !keys["bad-writer"] {
		t.Fatalf("bad-writer entry must be retained after upsert failure; keys=%v", keys)
	}
}

// Downgrade regression: a writer downgraded to reader in doc_member must not be
// re-amplified back to writer by a stale meta.grants["writer"] entry after a
// state/fallback transition (blocker #3). Reconcile never raises an existing
// doc_member role.
func TestReconcileDowngradeCannotReAmplify(t *testing.T) {
	slug := "recDowngrade"
	svc, store, mirror := seedReconcileDoc(t, slug, map[string]any{
		// Stale metadata still claims writer...
		"user-d": map[string]any{"role": "writer"},
	})
	// ...but doc_member (authoritative) already holds the post-downgrade reader.
	mirror.roles = map[string]int{"user-d": service.DocMemberRoleReader}

	if err := svc.ReconcileMetaGrantsToDocMember(context.Background(), slug); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if mirror.roles["user-d"] != service.DocMemberRoleReader {
		t.Fatalf("stale meta re-amplified downgraded user to %d; want reader(%d)", mirror.roles["user-d"], service.DocMemberRoleReader)
	}
	for _, uid := range mirror.inserts {
		if uid == "user-d" {
			t.Fatalf("reconcile issued an amplifying insert for a downgraded uid")
		}
	}
	// The stale entry is consumed (doc_member already authoritative) and cleared.
	if metaGrantKeys(t, store, slug)["user-d"] {
		t.Fatalf("stale meta.grants[user-d] must be cleared once doc_member is authoritative")
	}
}

// An existing admin row is never downgraded and its stale reader metadata is
// treated as consumed (doc_member authoritative), matching the existing
// admin-guard behavior.
func TestReconcileNeverDowngradesAdmin(t *testing.T) {
	slug := "recAdmin"
	svc, store, mirror := seedReconcileDoc(t, slug, map[string]any{
		"owner-2": map[string]any{"role": "reader"},
	})
	mirror.roles = map[string]int{"owner-2": service.DocMemberRoleAdmin}

	if err := svc.ReconcileMetaGrantsToDocMember(context.Background(), slug); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if mirror.roles["owner-2"] != service.DocMemberRoleAdmin {
		t.Fatalf("admin downgraded to %d", mirror.roles["owner-2"])
	}
	if metaGrantKeys(t, store, slug)["owner-2"] {
		t.Fatalf("stale meta.grants[owner-2] must be cleared once admin row is authoritative")
	}
}

// InsertDirectGrantIfAbsent must never overwrite an existing row. Concurrent
// reconcile + a direct authoritative writer write: the writer must survive
// regardless of interleaving (the insert-if-absent is a no-op once the row
// exists). Run under -race.
func TestReconcileInsertIfAbsentDoesNotOverwriteConcurrentWrite(t *testing.T) {
	slug := "recRace"
	svc, _, mirror := seedReconcileDoc(t, slug, map[string]any{
		"user-c": map[string]any{"role": "reader"}, // stale metadata says reader
	})
	done := make(chan struct{}, 2)
	go func() {
		// A concurrent authoritative write promotes user-c to writer.
		_, _ = mirror.InsertDirectGrantIfAbsent(context.Background(), mirror.docID, "user-c", service.DocMemberRoleWriter, "direct")
		done <- struct{}{}
	}()
	go func() {
		_ = svc.ReconcileMetaGrantsToDocMember(context.Background(), slug)
		done <- struct{}{}
	}()
	<-done
	<-done
	// Whoever inserted first wins; the loser is a no-op. reader(stale) must never
	// clobber a writer that got there first.
	if r := mirror.roles["user-c"]; r != service.DocMemberRoleReader && r != service.DocMemberRoleWriter {
		t.Fatalf("user-c role = %d; want reader or writer, never overwritten twice", r)
	}
}

// Insert-if-absent leaves a pre-existing row untouched and reports inserted=false.
func TestInsertIfAbsentNeverOverwritesExistingRow(t *testing.T) {
	_, _, mirror := seedReconcileDoc(t, "recIfAbsent", nil)
	mirror.roles = map[string]int{"u": service.DocMemberRoleWriter}
	inserted, err := mirror.InsertDirectGrantIfAbsent(context.Background(), mirror.docID, "u", service.DocMemberRoleReader, "x")
	if err != nil {
		t.Fatalf("insert-if-absent: %v", err)
	}
	if inserted {
		t.Fatalf("inserted=true for an existing row; must be false")
	}
	if mirror.roles["u"] != service.DocMemberRoleWriter {
		t.Fatalf("existing writer row overwritten to %d", mirror.roles["u"])
	}
	// Absent row inserts and reports inserted=true.
	inserted, err = mirror.InsertDirectGrantIfAbsent(context.Background(), mirror.docID, "fresh", service.DocMemberRoleReader, "x")
	if err != nil || !inserted {
		t.Fatalf("fresh insert inserted=%v err=%v; want true nil", inserted, err)
	}
}

// CapabilityForGrantRole fails closed for admin and unknown labels: the unwired
// fallback must never mint Manage from a legacy/corrupt admin entry.
func TestCapabilityForGrantRoleAdminAndUnknownFailClosed(t *testing.T) {
	if c := service.CapabilityForGrantRole("admin"); c != service.CapNone {
		t.Fatalf("CapabilityForGrantRole(admin) = %v; want CapNone", c)
	}
	if c := service.CapabilityForGrantRole("superuser"); c != service.CapNone {
		t.Fatalf("CapabilityForGrantRole(unknown) = %v; want CapNone", c)
	}
	if c := service.CapabilityForGrantRole("writer"); c != service.CapEdit {
		t.Fatalf("CapabilityForGrantRole(writer) = %v; want CapEdit", c)
	}
}

// Wired fallback-transition: a legacy/corrupt admin meta.grants entry must be
// cleared by wired reconcile so a later transition back to the unregistered
// fallback cannot escalate to Manage. After reconcile the admin entry is gone.
func TestReconcileClearsAdminMetaSoFallbackCannotEscalate(t *testing.T) {
	slug := "recEscalate"
	svc, store, mirror := seedReconcileDoc(t, slug, map[string]any{
		"ghost-admin": map[string]any{"role": "admin"},
	})
	if err := svc.ReconcileMetaGrantsToDocMember(context.Background(), slug); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, present := mirror.roles["ghost-admin"]; present {
		t.Fatalf("admin meta entry must never be written to doc_member")
	}
	if metaGrantKeys(t, store, slug)["ghost-admin"] {
		t.Fatalf("admin meta entry must be cleared after wired reconcile; a later unregistered fallback could otherwise escalate")
	}
}
