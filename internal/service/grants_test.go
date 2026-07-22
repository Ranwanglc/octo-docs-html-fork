package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

type mirrorCall struct {
	op        string
	docID     string
	uid       string
	role      int
	grantedBy string
}

type fakeDocMemberMirror struct {
	// mu guards calls/roles so tests that drive concurrent AuthService
	// callers (e.g. reconcile ∥ revoke) don't false-positive on -race for
	// mock-side map growth. Real mirrors get atomicity from their DB.
	mu         sync.Mutex
	calls      []mirrorCall
	docID      string
	resolveErr error
	err        error
	// roles keyed by uid feeds RoleByDocUID; listMembers feeds ListMembers.
	// Both are opt-in — an empty map behaves as "no rows".
	roles       map[string]int
	listMembers []service.DocMember
	readErr     error
	// unregistered=true makes DocIDBySlug return ok=false so tests can drive
	// the "doc not in doc_member yet" state (yujiawei round-3 P1a).
	unregistered bool
}

func (f *fakeDocMemberMirror) DocIDBySlug(_ context.Context, slug string) (string, bool, error) {
	if f.resolveErr != nil {
		return "", false, f.resolveErr
	}
	if f.unregistered {
		return "", false, nil
	}
	if f.docID == "" {
		return "node-" + slug, true, nil
	}
	return f.docID, true, nil
}

func (f *fakeDocMemberMirror) UpsertDirectGrant(_ context.Context, docID, uid string, role int, grantedBy string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, mirrorCall{op: "upsert", docID: docID, uid: uid, role: role, grantedBy: grantedBy})
	if f.err != nil {
		return f.err
	}
	if f.roles == nil {
		f.roles = map[string]int{}
	}
	// Mirror the P2 admin guard: a non-admin writer must not clobber an
	// existing admin row (the DB-side WHERE role<>3 lands the same invariant).
	if role != service.DocMemberRoleAdmin && f.roles[uid] == service.DocMemberRoleAdmin {
		return nil
	}
	f.roles[uid] = role
	return nil
}

func (f *fakeDocMemberMirror) DeleteGrant(_ context.Context, docID, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, mirrorCall{op: "delete", docID: docID, uid: uid})
	if f.err != nil {
		return f.err
	}
	// Mirror the P2 admin guard on DELETE: refuse admin rows so a concurrent
	// backfill promoting the row cannot be silently deleted.
	if f.roles[uid] == service.DocMemberRoleAdmin {
		return service.ErrDocMemberAdminGuard
	}
	delete(f.roles, uid)
	return nil
}

func (f *fakeDocMemberMirror) RoleByDocUID(_ context.Context, _, uid string) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return 0, false, f.readErr
	}
	if role, ok := f.roles[uid]; ok {
		return role, true, nil
	}
	return 0, false, nil
}

func (f *fakeDocMemberMirror) ListMembers(_ context.Context, _ string) ([]service.DocMember, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.listMembers, nil
}

// newGrantSvc builds an AuthService over an in-memory store seeded with one doc.
func newGrantSvc(t *testing.T) (*service.AuthService, string) {
	t.Helper()
	store := memory.New()
	slug := "docg"
	if err := store.PutMeta(context.Background(), slug, storage.DocMeta{Slug: slug, Title: "T"}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	return service.NewAuthService(store, &config.Config{}, sluglock.NewMemory()), slug
}

// GrantRole reads the per-uid role from Extra["grants"], nil-safe at every layer.
func TestDocMetaGrantRole(t *testing.T) {
	var nilMeta *storage.DocMeta
	if got := nilMeta.GrantRole("u"); got != "" { //nolint:staticcheck // legacy GrantRole shape test until A7 cleanup
		t.Fatalf("nil meta GrantRole = %q; want empty", got)
	}
	m := &storage.DocMeta{Extra: map[string]any{
		storage.GrantsExtraKey: map[string]any{ //nolint:staticcheck // legacy grants seed until A7 cleanup
			"friend": map[string]any{"role": "reader"},
		},
	}}
	if got := m.GrantRole("friend"); got != "reader" { //nolint:staticcheck // legacy GrantRole shape test until A7 cleanup
		t.Fatalf("GrantRole(friend) = %q; want reader", got)
	}
	if got := m.GrantRole("stranger"); got != "" { //nolint:staticcheck // legacy GrantRole shape test until A7 cleanup
		t.Fatalf("GrantRole(stranger) = %q; want empty", got)
	}
	if got := m.GrantRole(""); got != "" { //nolint:staticcheck // legacy GrantRole shape test until A7 cleanup
		t.Fatalf("GrantRole(\"\") = %q; want empty", got)
	}
}

// AddGrant upserts, ListGrants reads back, RemoveGrant is idempotent, and a
// non-reader role is rejected.
func TestGrantLifecycle(t *testing.T) {
	svc, slug := newGrantSvc(t)
	ctx := context.Background()

	if err := svc.AddGrant(ctx, slug, "u1", "reader", "owner"); err != nil {
		t.Fatalf("AddGrant: %v", err)
	}
	grants, err := svc.ListGrants(ctx, slug)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if grants["u1"] != "reader" {
		t.Fatalf("grants[u1] = %q; want reader", grants["u1"])
	}

	if err := svc.AddGrant(ctx, slug, "u1", "writer", "owner"); err == nil {
		t.Fatalf("AddGrant writer should be rejected")
	}
	if err := svc.AddGrant(ctx, slug, "", "reader", "owner"); err == nil {
		t.Fatalf("AddGrant empty uid should be rejected")
	}

	if err := svc.RemoveGrant(ctx, slug, "u1"); err != nil {
		t.Fatalf("RemoveGrant: %v", err)
	}
	if err := svc.RemoveGrant(ctx, slug, "u1"); err != nil {
		t.Fatalf("RemoveGrant idempotent: %v", err)
	}
	grants, _ = svc.ListGrants(ctx, slug)
	if len(grants) != 0 {
		t.Fatalf("grants after remove = %v; want empty", grants)
	}
}

func TestGrantMirrorsDocMember(t *testing.T) {
	svc, slug := newGrantSvc(t)
	mirror := &fakeDocMemberMirror{docID: "doc-node-1"}
	svc.WithDocMemberMirror(mirror)
	ctx := context.Background()

	if err := svc.AddGrant(ctx, slug, "u1", "reader", "owner"); err != nil {
		t.Fatalf("AddGrant: %v", err)
	}
	if len(mirror.calls) != 1 {
		t.Fatalf("mirror calls after AddGrant = %d; want 1", len(mirror.calls))
	}
	if got := mirror.calls[0]; got != (mirrorCall{op: "upsert", docID: "doc-node-1", uid: "u1", role: 1, grantedBy: "owner"}) {
		t.Fatalf("AddGrant mirror call = %+v", got)
	}

	if err := svc.RemoveGrant(ctx, slug, "u1"); err != nil {
		t.Fatalf("RemoveGrant: %v", err)
	}
	if len(mirror.calls) != 2 {
		t.Fatalf("mirror calls after RemoveGrant = %d; want 2", len(mirror.calls))
	}
	if got := mirror.calls[1]; got != (mirrorCall{op: "delete", docID: "doc-node-1", uid: "u1"}) {
		t.Fatalf("RemoveGrant mirror call = %+v", got)
	}
}

func TestRemoveAbsentGrantSkipsMirror(t *testing.T) {
	svc, slug := newGrantSvc(t)
	mirror := &fakeDocMemberMirror{docID: "doc-node-1"}
	svc.WithDocMemberMirror(mirror)
	ctx := context.Background()

	// Removing a uid that was never granted is a no-op and must not mirror
	// (no empty permission_epoch bump).
	if err := svc.RemoveGrant(ctx, slug, "never-granted"); err != nil {
		t.Fatalf("RemoveGrant absent: %v", err)
	}
	if len(mirror.calls) != 0 {
		t.Fatalf("mirror calls after absent remove = %d; want 0", len(mirror.calls))
	}
}

// Plan③ A6 flipped the source-of-truth: doc_member is now authoritative for
// direct grants, so a mirror write error must surface to the caller instead
// of being swallowed (silent divergence between what the API reports and
// what the auth layer will read on the next request).
//
// P1-B changed AddGrant to no-op when uid is already reader (idempotent), so
// this test exercises the write path with a fresh uid and a pre-seeded uid
// separately: AddGrant on a new uid triggers upsert (surfaces error), and
// RemoveGrant on a reader still calls DeleteGrant (surfaces error).
func TestGrantMirrorErrorsSurface(t *testing.T) {
	svc, slug := newGrantSvc(t)
	svc.WithDocMemberMirror(&fakeDocMemberMirror{err: errors.New("mirror down"), roles: map[string]int{"u1": 1}})
	ctx := context.Background()

	// Fresh uid → probe misses → upsert runs → mirror error surfaces.
	if err := svc.AddGrant(ctx, slug, "fresh-uid", "reader", "owner"); err == nil {
		t.Fatalf("AddGrant with mirror error = nil; want error")
	}
	if err := svc.RemoveGrant(ctx, slug, "u1"); err == nil {
		t.Fatalf("RemoveGrant with mirror error = nil; want error")
	}
}

func TestGrantNilMirrorNoops(t *testing.T) {
	svc, slug := newGrantSvc(t)
	ctx := context.Background()

	if err := svc.AddGrant(ctx, slug, "u1", "reader", "owner"); err != nil {
		t.Fatalf("AddGrant with nil mirror: %v", err)
	}
	if err := svc.RemoveGrant(ctx, slug, "u1"); err != nil {
		t.Fatalf("RemoveGrant with nil mirror: %v", err)
	}
}

// ListGrants on a missing doc is NotFound (hides existence).
func TestListGrantsMissingDoc(t *testing.T) {
	svc, _ := newGrantSvc(t)
	_, err := svc.ListGrants(context.Background(), "no-such-slug")
	if _, ok := err.(*apperr.Error); !ok {
		t.Fatalf("ListGrants missing = %v; want apperr", err)
	}
}

// A5: RoleByDocUID contract — hit returns the row role and ok=true, miss returns
// ok=false with nil error, and a read error surfaces to the caller. Mirror
// callers rely on this shape to distinguish "no grant" from "lookup failed".
func TestFakeMirrorRoleByDocUIDShape(t *testing.T) {
	m := &fakeDocMemberMirror{roles: map[string]int{"alice": 3, "bob": 1}}
	ctx := context.Background()
	if r, ok, err := m.RoleByDocUID(ctx, "d1", "alice"); err != nil || !ok || r != 3 {
		t.Fatalf("RoleByDocUID(alice) = (%d,%v,%v); want (3,true,nil)", r, ok, err)
	}
	if r, ok, err := m.RoleByDocUID(ctx, "d1", "stranger"); err != nil || ok || r != 0 {
		t.Fatalf("RoleByDocUID(stranger) = (%d,%v,%v); want (0,false,nil)", r, ok, err)
	}
	m.readErr = errors.New("db down")
	if _, _, err := m.RoleByDocUID(ctx, "d1", "alice"); err == nil {
		t.Fatalf("RoleByDocUID with readErr = nil; want error")
	}
}

// A5: ListMembers contract — returns the seeded rows verbatim so grants.List
// can render (uid,role,granted_by) tuples; a read error surfaces (never a
// partial slice with err).
func TestFakeMirrorListMembersShape(t *testing.T) {
	seed := []service.DocMember{
		{UID: "creator", Role: 3, GrantedBy: "system"},
		{UID: "friend", Role: 1, GrantedBy: "creator"},
	}
	m := &fakeDocMemberMirror{listMembers: seed}
	ctx := context.Background()
	got, err := m.ListMembers(ctx, "d1")
	if err != nil {
		t.Fatalf("ListMembers err: %v", err)
	}
	if len(got) != 2 || got[0].UID != "creator" || got[0].Role != 3 || got[1].UID != "friend" {
		t.Fatalf("ListMembers = %+v; want seed", got)
	}
	m.readErr = errors.New("db down")
	if _, err := m.ListMembers(ctx, "d1"); err == nil {
		t.Fatalf("ListMembers with readErr = nil; want error")
	}
}

// newGrantSvcWithCreator seeds the doc with a creator_uid so RemoveGrant's
// creator-protection tier has a value to compare against.
//
//nolint:unparam // creator kept parametric so P1-B AddGrant creator-uid tests can vary it.
func newGrantSvcWithCreator(t *testing.T, creator string) (*service.AuthService, string) {
	t.Helper()
	store := memory.New()
	slug := "doca"
	meta := storage.DocMeta{Slug: slug, Title: "T", Extra: map[string]any{storage.CreatorUIDExtraKey: creator}}
	if err := store.PutMeta(context.Background(), slug, meta); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	return service.NewAuthService(store, &config.Config{}, sluglock.NewMemory()), slug
}

// A6: AddGrant is idempotent — same (uid,role) applied twice is not an error
// and does not produce a duplicate role assignment. Two mirror upsert calls
// are expected (the mirror itself enforces uniqueness), but the resulting
// state is a single reader row.
func TestAddGrantIdempotent(t *testing.T) {
	svc, slug := newGrantSvc(t)
	mirror := &fakeDocMemberMirror{docID: "doc-A"}
	svc.WithDocMemberMirror(mirror)
	ctx := context.Background()

	if err := svc.AddGrant(ctx, slug, "u1", "reader", "owner"); err != nil {
		t.Fatalf("first AddGrant: %v", err)
	}
	if err := svc.AddGrant(ctx, slug, "u1", "reader", "owner"); err != nil {
		t.Fatalf("second AddGrant: %v", err)
	}
	if got := mirror.roles["u1"]; got != service.DocMemberRoleReader {
		t.Fatalf("roles[u1] = %d; want reader(%d)", got, service.DocMemberRoleReader)
	}
	if len(mirror.roles) != 1 {
		t.Fatalf("roles = %v; want exactly one entry", mirror.roles)
	}
}

// A6: RemoveGrant refuses to revoke the doc's creator_uid — the grants API is
// reader-scoped and must never touch the author identity.
func TestRemoveGrantProtectsCreator(t *testing.T) {
	svc, slug := newGrantSvcWithCreator(t, "creator-uid")
	svc.WithDocMemberMirror(&fakeDocMemberMirror{docID: "doc-A"})
	ctx := context.Background()

	if err := svc.RemoveGrant(ctx, slug, "creator-uid"); !errors.Is(err, service.ErrGrantProtected) {
		t.Fatalf("RemoveGrant(creator) = %v; want ErrGrantProtected", err)
	}
}

// A6: RemoveGrant refuses to revoke a doc_member admin row (role=3) — the
// M1 backfill lands owner rows here and they must survive a rogue revoke.
func TestRemoveGrantProtectsDocMemberAdmin(t *testing.T) {
	svc, slug := newGrantSvc(t)
	svc.WithDocMemberMirror(&fakeDocMemberMirror{
		docID: "doc-A",
		roles: map[string]int{"owner-uid": service.DocMemberRoleAdmin},
	})
	ctx := context.Background()

	if err := svc.RemoveGrant(ctx, slug, "owner-uid"); !errors.Is(err, service.ErrGrantProtected) {
		t.Fatalf("RemoveGrant(admin) = %v; want ErrGrantProtected", err)
	}
}

// A6: RemoveGrant on a regular reader row succeeds and clears the row.
func TestRemoveGrantRevokesReader(t *testing.T) {
	svc, slug := newGrantSvc(t)
	mirror := &fakeDocMemberMirror{
		docID: "doc-A",
		roles: map[string]int{"u1": service.DocMemberRoleReader},
	}
	svc.WithDocMemberMirror(mirror)
	ctx := context.Background()

	if err := svc.RemoveGrant(ctx, slug, "u1"); err != nil {
		t.Fatalf("RemoveGrant(reader): %v", err)
	}
	if _, still := mirror.roles["u1"]; still {
		t.Fatalf("roles[u1] still present after revoke: %v", mirror.roles)
	}
}

// P2-B: ListGrants drops the creator row from the wired path. The HTTP
// handler synthesises the leading {creator, "author", "owner"} entry itself,
// so surfacing the same uid here caused a duplicate row in the UI that
// rendered as a deletable grant (and 409-ed on click). Only non-creator
// members should come back.
func TestListGrantsSkipsCreatorInDocMember(t *testing.T) {
	svc, slug := newGrantSvcWithCreator(t, "creator-uid")
	// M1 backfilled the creator's admin row + one reader friend.
	svc.WithDocMemberMirror(&fakeDocMemberMirror{
		docID: "doc-A",
		listMembers: []service.DocMember{
			{UID: "creator-uid", Role: service.DocMemberRoleAdmin, GrantedBy: "system"},
			{UID: "friend-uid", Role: service.DocMemberRoleReader, GrantedBy: "creator-uid"},
		},
	})
	ctx := context.Background()

	got, err := svc.ListGrants(ctx, slug)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if _, ok := got["creator-uid"]; ok {
		t.Fatalf("creator uid must not appear in ListGrants (handler synthesises it); got %v", got)
	}
	if got["friend-uid"] != "reader" {
		t.Fatalf("friend role = %q; want reader", got["friend-uid"])
	}
	if len(got) != 1 {
		t.Fatalf("grants = %v; want exactly the friend entry", got)
	}
}

// P2-B: with the creator dropped from the wired path, an empty doc_member
// list produces an empty grants map. The handler still renders the creator
// row from meta.creator_uid so the UI is unaffected.
func TestListGrantsEmptyWhenNoMembersAndCreatorSuppressed(t *testing.T) {
	svc, slug := newGrantSvcWithCreator(t, "creator-uid")
	svc.WithDocMemberMirror(&fakeDocMemberMirror{docID: "doc-A"})
	ctx := context.Background()

	got, err := svc.ListGrants(ctx, slug)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("grants = %v; want empty (handler synthesises creator)", got)
	}
}

// A6+P1-B: AddGrant refuses to downgrade a doc_member admin (role=3) to
// reader. UpsertDirectGrant would otherwise clobber the admin row via
// ON DUPLICATE KEY UPDATE — after Stage 5 A1 this is the owner's only author
// signal, so the protection is a hard prerequisite.
func TestAddGrantRefusesDowngradeAdmin(t *testing.T) {
	svc, slug := newGrantSvc(t)
	mirror := &fakeDocMemberMirror{
		docID: "doc-A",
		roles: map[string]int{"owner-uid": service.DocMemberRoleAdmin},
	}
	svc.WithDocMemberMirror(mirror)
	ctx := context.Background()

	if err := svc.AddGrant(ctx, slug, "owner-uid", "reader", "someone"); !errors.Is(err, service.ErrGrantProtected) {
		t.Fatalf("AddGrant(admin) = %v; want ErrGrantProtected", err)
	}
	// Admin row untouched — no upsert call recorded.
	for _, c := range mirror.calls {
		if c.op == "upsert" && c.uid == "owner-uid" {
			t.Fatalf("admin row was upserted: %+v", c)
		}
	}
	if mirror.roles["owner-uid"] != service.DocMemberRoleAdmin {
		t.Fatalf("admin role after refused downgrade = %d; want %d", mirror.roles["owner-uid"], service.DocMemberRoleAdmin)
	}
}

// A6+P1-B: AddGrant refuses to write a reader row on the creator uid. The
// creator's author identity flows from meta.creator_uid + doc_member admin,
// never from a reader grant, so a reader row on the creator is nonsensical
// and would either land as a duplicate or (after A1) demote the owner.
func TestAddGrantRefusesCreator(t *testing.T) {
	svc, slug := newGrantSvcWithCreator(t, "creator-uid")
	mirror := &fakeDocMemberMirror{docID: "doc-A"}
	svc.WithDocMemberMirror(mirror)
	ctx := context.Background()

	if err := svc.AddGrant(ctx, slug, "creator-uid", "reader", "someone"); !errors.Is(err, service.ErrGrantProtected) {
		t.Fatalf("AddGrant(creator) = %v; want ErrGrantProtected", err)
	}
	if len(mirror.calls) != 0 {
		t.Fatalf("creator refuse should not touch mirror; calls=%+v", mirror.calls)
	}
}

// A6+P1-B: AddGrant on a uid that is already a reader is a no-op — no
// duplicate upsert and no permission_epoch bump. Guards against churn when
// the UI resends the same grant on save.
func TestAddGrantIdempotentReaderNoEpochBump(t *testing.T) {
	svc, slug := newGrantSvc(t)
	mirror := &fakeDocMemberMirror{
		docID: "doc-A",
		roles: map[string]int{"reader-uid": service.DocMemberRoleReader},
	}
	svc.WithDocMemberMirror(mirror)
	ctx := context.Background()

	if err := svc.AddGrant(ctx, slug, "reader-uid", "reader", "owner"); err != nil {
		t.Fatalf("AddGrant(existing reader): %v", err)
	}
	if len(mirror.calls) != 0 {
		t.Fatalf("idempotent reader path should not touch mirror; calls=%+v", mirror.calls)
	}
	if mirror.roles["reader-uid"] != service.DocMemberRoleReader {
		t.Fatalf("reader role changed: %d", mirror.roles["reader-uid"])
	}
}

// P2-C: RemoveGrant on the wired path, when the doc has no rich-doc row yet
// (DocIDBySlug returns ok=false), sweeps any legacy meta.grants[uid] so a
// later unwire or a migration reading meta.grants cannot resurrect a stale
// grant. Reads already ignore meta.grants when the mirror is wired, so this
// is bookkeeping only — the response is nil either way.
func TestRemoveGrantWiredButUnregisteredCleansMeta(t *testing.T) {
	store := memory.New()
	slug := "docP2C"
	// Seed the doc and a legacy meta.grants[friend]=reader row.
	if err := store.PutMeta(context.Background(), slug, storage.DocMeta{
		Slug:  slug,
		Title: "T",
		Extra: map[string]any{
			storage.GrantsExtraKey: map[string]any{ //nolint:staticcheck // legacy meta.grants seed for P2-C sweep test
				"friend": map[string]any{"role": "reader"},
			},
		},
	}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	svc := service.NewAuthService(store, &config.Config{}, sluglock.NewMemory())
	// Wire an unregisteredMirror that always returns ok=false so RemoveGrant
	// hits the P2-C sweep branch instead of the doc_member DELETE path.
	svc.WithDocMemberMirror(&unregisteredMirror{})
	ctx := context.Background()

	if err := svc.RemoveGrant(ctx, slug, "friend"); err != nil {
		t.Fatalf("RemoveGrant(unregistered): %v", err)
	}
	// Reads land on the wired path (ListMembers → nil), so use the storage
	// layer to prove meta.grants was actually swept.
	meta, err := store.GetMeta(ctx, slug)
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	grants, _ := meta.Extra[storage.GrantsExtraKey].(map[string]any) //nolint:staticcheck // reading legacy meta.grants to assert sweep
	if _, still := grants["friend"]; still {
		t.Fatalf("legacy meta.grants[friend] not swept: %v", grants)
	}
}

// unregisteredMirror is a DocMemberMirror whose DocIDBySlug always returns
// ok=false — simulating a wired-but-unregistered doc for the P2-C sweep
// path. Every other method is unreachable in that state and panics to
// catch accidental calls.
type unregisteredMirror struct{}

func (unregisteredMirror) DocIDBySlug(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (unregisteredMirror) UpsertDirectGrant(context.Context, string, string, int, string) error {
	panic("unregisteredMirror.UpsertDirectGrant should not be called")
}

func (unregisteredMirror) DeleteGrant(context.Context, string, string) error {
	panic("unregisteredMirror.DeleteGrant should not be called")
}

func (unregisteredMirror) RoleByDocUID(context.Context, string, string) (int, bool, error) {
	panic("unregisteredMirror.RoleByDocUID should not be called")
}

func (unregisteredMirror) ListMembers(context.Context, string) ([]service.DocMember, error) {
	return nil, nil
}

// yujiawei round-3 P1a: RoleBySlugUID must distinguish two "ok=false" states
// so bestCred can gate its meta fallback correctly. docRegistered=false means
// "doc has no rich-doc row yet — legacy fallback allowed"; docRegistered=true
// with ok=false means "doc IS registered, uid just has no row — do NOT fall
// back to meta.grants, or a stale entry after DELETE would grant read again".

func TestRoleBySlugUIDUnwiredReportsUnregistered(t *testing.T) {
	svc, slug := newGrantSvc(t)
	role, ok, docRegistered, err := svc.RoleBySlugUID(context.Background(), slug, "u1")
	if err != nil || role != 0 || ok || docRegistered {
		t.Fatalf("unwired: got (role=%d ok=%v docRegistered=%v err=%v); want (0 false false nil)", role, ok, docRegistered, err)
	}
}

func TestRoleBySlugUIDDocUnregisteredReportsUnregistered(t *testing.T) {
	svc, slug := newGrantSvc(t)
	svc.WithDocMemberMirror(&fakeDocMemberMirror{unregistered: true})
	role, ok, docRegistered, err := svc.RoleBySlugUID(context.Background(), slug, "u1")
	if err != nil || role != 0 || ok || docRegistered {
		t.Fatalf("wired-but-unregistered: got (role=%d ok=%v docRegistered=%v err=%v); want (0 false false nil)", role, ok, docRegistered, err)
	}
}

func TestRoleBySlugUIDRegisteredNoRowReportsRegistered(t *testing.T) {
	svc, slug := newGrantSvc(t)
	svc.WithDocMemberMirror(&fakeDocMemberMirror{docID: "d1"}) // registered, roles empty
	role, ok, docRegistered, err := svc.RoleBySlugUID(context.Background(), slug, "ghost")
	if err != nil || role != 0 || ok || !docRegistered {
		t.Fatalf("registered-no-row: got (role=%d ok=%v docRegistered=%v err=%v); want (0 false true nil) — P1a revoke-bypass gate", role, ok, docRegistered, err)
	}
}

func TestRoleBySlugUIDRegisteredHitReturnsRole(t *testing.T) {
	svc, slug := newGrantSvc(t)
	svc.WithDocMemberMirror(&fakeDocMemberMirror{docID: "d1", roles: map[string]int{"reader-1": service.DocMemberRoleReader}})
	role, ok, docRegistered, err := svc.RoleBySlugUID(context.Background(), slug, "reader-1")
	if err != nil || role != service.DocMemberRoleReader || !ok || !docRegistered {
		t.Fatalf("registered-hit: got (role=%d ok=%v docRegistered=%v err=%v); want (reader true true nil)", role, ok, docRegistered, err)
	}
}

// yujiawei round-3 P2: AddGrant on an unregistered doc must fall back to
// meta.grants, matching reads / ListGrants / RemoveGrant. Prior to this fix
// it 404'd, which made thread-mount / non-mounted docs (which never register
// in doc_member) permanently un-grantable while still readable — an
// asymmetric API surface.
func TestAddGrantUnregisteredFallsBackToMeta(t *testing.T) {
	svc, slug := newGrantSvc(t)
	svc.WithDocMemberMirror(&fakeDocMemberMirror{unregistered: true})

	if err := svc.AddGrant(context.Background(), slug, "reader-9", "reader", "granter"); err != nil {
		t.Fatalf("AddGrant on unregistered doc: got %v; want nil (meta fallback)", err)
	}

	// Verify: ListGrants (unregistered path also reads meta) surfaces reader-9.
	grants, err := svc.ListGrants(context.Background(), slug)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if got := grants["reader-9"]; got != "reader" {
		t.Fatalf("meta.grants[reader-9] = %q; want reader (AddGrant did not fall back)", got)
	}
}

// yujiawei round-4 P1: grants issued during the pre-registration gap land in
// meta.grants (AddGrant unregistered fallback). Once afterPublished registers
// the doc, the strict wired A4 gate refuses to serve them from meta.grants,
// so they must be reconciled into doc_member or the reader silently 404s.
func TestReconcileMetaGrantsPromotesReaderIntoDocMember(t *testing.T) {
	store := memory.New()
	slug := "docReconcile"
	if err := store.PutMeta(context.Background(), slug, storage.DocMeta{
		Slug:  slug,
		Title: "T",
		Extra: map[string]any{
			storage.CreatorUIDExtraKey: "owner-1",
			storage.GrantsExtraKey: map[string]any{ //nolint:staticcheck // seed legacy grants for reconcile
				"reader-5": map[string]any{"role": "reader", "granted_by": "owner-1"},
			},
		},
	}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	svc := service.NewAuthService(store, &config.Config{}, sluglock.NewMemory())
	mirror := &fakeDocMemberMirror{docID: "doc-R"}
	svc.WithDocMemberMirror(mirror)

	if err := svc.ReconcileMetaGrantsToDocMember(context.Background(), slug); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if mirror.roles["reader-5"] != service.DocMemberRoleReader {
		t.Fatalf("reader-5 not promoted into doc_member: roles=%v", mirror.roles)
	}
	// meta.grants entry stays put so mirror-unwired deploys keep working.
	meta, _ := store.GetMeta(context.Background(), slug)
	grants, _ := meta.Extra[storage.GrantsExtraKey].(map[string]any) //nolint:staticcheck // asserting meta.grants left in place
	if _, ok := grants["reader-5"]; !ok {
		t.Fatalf("meta.grants[reader-5] must not be deleted; got %v", grants)
	}
}

// Reconcile must skip the creator uid (creator's admin row is provisioned by
// M1, not by legacy meta.grants) and must skip non-reader roles / malformed
// entries so a rogue value can't surprise-promote.
func TestReconcileMetaGrantsSkipsCreatorAndNonReader(t *testing.T) {
	store := memory.New()
	slug := "docFilter"
	if err := store.PutMeta(context.Background(), slug, storage.DocMeta{
		Slug:  slug,
		Title: "T",
		Extra: map[string]any{
			storage.CreatorUIDExtraKey: "owner-1",
			storage.GrantsExtraKey: map[string]any{ //nolint:staticcheck // seed mixed legacy rows
				"owner-1":     map[string]any{"role": "reader"}, // creator skip
				"weirdo":      map[string]any{"role": "editor"}, // unknown role skip
				"malformed":   "not-a-map",                      // wrong shape skip
				"real-reader": map[string]any{"role": "reader"},
			},
		},
	}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	svc := service.NewAuthService(store, &config.Config{}, sluglock.NewMemory())
	mirror := &fakeDocMemberMirror{docID: "doc-F"}
	svc.WithDocMemberMirror(mirror)

	if err := svc.ReconcileMetaGrantsToDocMember(context.Background(), slug); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, present := mirror.roles["owner-1"]; present {
		t.Fatalf("creator uid must be skipped, but got role %d", mirror.roles["owner-1"])
	}
	if _, present := mirror.roles["weirdo"]; present {
		t.Fatalf("non-reader role must be skipped, but got role %d", mirror.roles["weirdo"])
	}
	if _, present := mirror.roles["malformed"]; present {
		t.Fatal("malformed entry must be skipped")
	}
	if mirror.roles["real-reader"] != service.DocMemberRoleReader {
		t.Fatalf("real reader not promoted: roles=%v", mirror.roles)
	}
}

// Reconcile is a no-op when doc has no legacy meta.grants (the common case).
func TestReconcileMetaGrantsNoOpEmpty(t *testing.T) {
	store := memory.New()
	slug := "docEmpty"
	if err := store.PutMeta(context.Background(), slug, storage.DocMeta{Slug: slug, Title: "T", Extra: map[string]any{storage.CreatorUIDExtraKey: "owner-1"}}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	svc := service.NewAuthService(store, &config.Config{}, sluglock.NewMemory())
	mirror := &fakeDocMemberMirror{docID: "doc-E"}
	svc.WithDocMemberMirror(mirror)

	if err := svc.ReconcileMetaGrantsToDocMember(context.Background(), slug); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(mirror.calls) != 0 {
		t.Fatalf("reconcile called mirror on empty grants: %+v", mirror.calls)
	}
}

// Reconcile with an unwired mirror is a silent no-op (single-node deploys).
func TestReconcileMetaGrantsUnwiredNoop(t *testing.T) {
	svc, slug := newGrantSvc(t)
	// Add a legacy row that would otherwise be reconciled.
	meta, _ := memory.New().GetMeta(context.Background(), slug)
	_ = meta // meta unused; unwired path returns before touching store beyond GetMeta
	if err := svc.ReconcileMetaGrantsToDocMember(context.Background(), slug); err != nil {
		t.Fatalf("unwired reconcile must be nil: %v", err)
	}
}

// yujiawei round-4 P2: on the registered branch, RemoveGrant must also sweep
// any stale meta.grants[uid] entry (M2 copies rather than moves), otherwise a
// later unmount / soft-delete flipping DocIDBySlug back to ok=false would let
// the A4 fallback resurrect read access after a revoke.
func TestRemoveGrantRegisteredBranchAlsoSweepsMeta(t *testing.T) {
	store := memory.New()
	slug := "docP2Reg"
	// Seed the doc with a legacy meta.grants[reader-9]=reader row alongside
	// a live doc_member row (via the fake mirror below).
	if err := store.PutMeta(context.Background(), slug, storage.DocMeta{
		Slug:  slug,
		Title: "T",
		Extra: map[string]any{
			storage.CreatorUIDExtraKey: "owner-1",
			storage.GrantsExtraKey: map[string]any{ //nolint:staticcheck // seed M2-style double-write
				"reader-9": map[string]any{"role": "reader"},
			},
		},
	}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	svc := service.NewAuthService(store, &config.Config{}, sluglock.NewMemory())
	mirror := &fakeDocMemberMirror{docID: "doc-P2Reg", roles: map[string]int{"reader-9": service.DocMemberRoleReader}}
	svc.WithDocMemberMirror(mirror)

	if err := svc.RemoveGrant(context.Background(), slug, "reader-9"); err != nil {
		t.Fatalf("RemoveGrant(registered): %v", err)
	}
	// doc_member row was deleted...
	if _, still := mirror.roles["reader-9"]; still {
		t.Fatalf("doc_member row not deleted: roles=%v", mirror.roles)
	}
	// ...and the stale meta.grants entry was swept too.
	meta, _ := store.GetMeta(context.Background(), slug)
	grants, _ := meta.Extra[storage.GrantsExtraKey].(map[string]any) //nolint:staticcheck // reading legacy meta.grants to assert sweep
	if _, still := grants["reader-9"]; still {
		t.Fatalf("meta.grants[reader-9] not swept on registered branch: %v", grants)
	}
}

// Sweep is a no-op when meta.grants has nothing for uid (probe returns nil,
// no permission_epoch bump). Guards against a doubled sweep after the
// unregistered branch already ran removeGrantFromMeta itself.
func TestRemoveGrantSweepIdempotentOnAbsentMeta(t *testing.T) {
	store := memory.New()
	slug := "docP2Idem"
	if err := store.PutMeta(context.Background(), slug, storage.DocMeta{Slug: slug, Title: "T", Extra: map[string]any{storage.CreatorUIDExtraKey: "owner-1"}}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	svc := service.NewAuthService(store, &config.Config{}, sluglock.NewMemory())
	svc.WithDocMemberMirror(&fakeDocMemberMirror{docID: "doc-P2Idem", roles: map[string]int{"friend": service.DocMemberRoleReader}})

	if err := svc.RemoveGrant(context.Background(), slug, "friend"); err != nil {
		t.Fatalf("RemoveGrant(no meta grant): %v", err)
	}
	// No panic, no error — that's the assertion.
}

// yujiawei round-4 P2 race guard: Upsert must preserve an admin row when the
// caller writes a non-admin role. Simulates a backfill promoting owner-2 to
// admin between AddGrant's probe and the mirror write; the SQL WHERE role<>3
// invariant (fakeDocMemberMirror mirrors it) keeps the admin.
func TestUpsertDirectGrantPreservesAdminOnNonAdminWrite(t *testing.T) {
	mirror := &fakeDocMemberMirror{docID: "doc-P2", roles: map[string]int{"owner-2": service.DocMemberRoleAdmin}}
	// Direct call so we exercise the mirror-level guard without going through
	// AddGrant's probe (that already checks admin before we get here).
	if err := mirror.UpsertDirectGrant(context.Background(), "doc-P2", "owner-2", service.DocMemberRoleReader, "attacker"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if mirror.roles["owner-2"] != service.DocMemberRoleAdmin {
		t.Fatalf("admin row clobbered: %d (want %d)", mirror.roles["owner-2"], service.DocMemberRoleAdmin)
	}
}

// M1-style admin backfill still succeeds: when the caller IS writing admin,
// the guard steps aside so the promote lands.
func TestUpsertDirectGrantAdminWriterPromotes(t *testing.T) {
	mirror := &fakeDocMemberMirror{docID: "doc-M1", roles: map[string]int{"owner-2": service.DocMemberRoleReader}}
	if err := mirror.UpsertDirectGrant(context.Background(), "doc-M1", "owner-2", service.DocMemberRoleAdmin, "m1"); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}
	if mirror.roles["owner-2"] != service.DocMemberRoleAdmin {
		t.Fatalf("admin promote lost: %d", mirror.roles["owner-2"])
	}
}

// yujiawei round-4 P2 race guard: DeleteGrant must refuse an admin row and
// return ErrDocMemberAdminGuard so callers surface a protected error rather
// than silently deleting an admin promoted between probe and DELETE.
func TestDeleteGrantRefusesAdminRow(t *testing.T) {
	mirror := &fakeDocMemberMirror{docID: "doc-P2D", roles: map[string]int{"owner-2": service.DocMemberRoleAdmin}}
	err := mirror.DeleteGrant(context.Background(), "doc-P2D", "owner-2")
	if !errors.Is(err, service.ErrDocMemberAdminGuard) {
		t.Fatalf("DeleteGrant(admin) = %v; want ErrDocMemberAdminGuard", err)
	}
	if mirror.roles["owner-2"] != service.DocMemberRoleAdmin {
		t.Fatalf("admin row deleted despite guard: %d", mirror.roles["owner-2"])
	}
}

// RemoveGrant surfaces ErrGrantProtected when the mirror's DELETE races with
// an admin promotion. Exercises the wired path so the probe misses the
// admin (probe sees "reader" seed) but DeleteGrant sees "admin" (test
// simulates the race by pre-seeding the mirror as admin — the probe's stale
// reader view lives in the roles-arg of the fakeDocMemberMirror).
func TestRemoveGrantSurfacesRaceAsProtected(t *testing.T) {
	store := memory.New()
	slug := "docRace"
	if err := store.PutMeta(context.Background(), slug, storage.DocMeta{
		Slug:  slug,
		Title: "T",
		Extra: map[string]any{storage.CreatorUIDExtraKey: "owner-1"},
	}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	svc := service.NewAuthService(store, &config.Config{}, sluglock.NewMemory())
	// Use raceMirror so the probe returns "reader" but the DELETE hits an
	// admin row and refuses. Matches the P2 SQL guard flip.
	svc.WithDocMemberMirror(&raceMirror{docID: "doc-R", probeRole: service.DocMemberRoleReader, delRole: service.DocMemberRoleAdmin})

	err := svc.RemoveGrant(context.Background(), slug, "owner-2")
	if !errors.Is(err, service.ErrGrantProtected) {
		t.Fatalf("RemoveGrant race = %v; want ErrGrantProtected", err)
	}
}

// raceMirror lets a test decouple RoleByDocUID (probe) from DeleteGrant so it
// can simulate a backfill that lands between the two calls.
type raceMirror struct {
	docID     string
	probeRole int
	delRole   int
}

func (r *raceMirror) DocIDBySlug(context.Context, string) (string, bool, error) {
	return r.docID, true, nil
}
func (r *raceMirror) UpsertDirectGrant(context.Context, string, string, int, string) error {
	panic("raceMirror.UpsertDirectGrant should not be called")
}
func (r *raceMirror) DeleteGrant(context.Context, string, string) error {
	if r.delRole == service.DocMemberRoleAdmin {
		return service.ErrDocMemberAdminGuard
	}
	return nil
}
func (r *raceMirror) RoleByDocUID(context.Context, string, string) (int, bool, error) {
	return r.probeRole, true, nil
}
func (r *raceMirror) ListMembers(context.Context, string) ([]service.DocMember, error) {
	return nil, nil
}

// P1 admin-not-clobber assertion (deferred from commit 1 until this commit
// lands the mirror-side guard): if M1 has already promoted owner-2 to admin
// and a stale meta.grants["owner-2"]="reader" is still around, reconcile
// must NOT downgrade the admin. The fakeDocMemberMirror now mirrors the P2
// SQL guard so this test passes with the same seed.
func TestReconcileMetaGrantsSkipsAdminRow(t *testing.T) {
	store := memory.New()
	slug := "docAdminGuard"
	if err := store.PutMeta(context.Background(), slug, storage.DocMeta{
		Slug:  slug,
		Title: "T",
		Extra: map[string]any{
			storage.CreatorUIDExtraKey: "owner-1",
			storage.GrantsExtraKey: map[string]any{ //nolint:staticcheck // seed stale legacy row
				"owner-2": map[string]any{"role": "reader"},
			},
		},
	}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	svc := service.NewAuthService(store, &config.Config{}, sluglock.NewMemory())
	mirror := &fakeDocMemberMirror{docID: "doc-A", roles: map[string]int{"owner-2": service.DocMemberRoleAdmin}}
	svc.WithDocMemberMirror(mirror)

	if err := svc.ReconcileMetaGrantsToDocMember(context.Background(), slug); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if mirror.roles["owner-2"] != service.DocMemberRoleAdmin {
		t.Fatalf("admin row clobbered by reconcile: %d (want %d)", mirror.roles["owner-2"], service.DocMemberRoleAdmin)
	}
}

// yujiawei round-5 P1-b: reconcile ∥ revoke TOCTOU. Without the slug lock,
// reconcile snapshots meta.grants[uid], then RemoveGrant deletes the
// doc_member row and sweeps meta.grants, and finally reconcile's stale
// snapshot re-inserts the doc_member row = resurrected revoked grant.
//
// With the lock, RemoveGrant serializes against reconcile: whichever runs
// first fully drains the lock before the other observes state. When
// RemoveGrant lands its meta sweep first, reconcile's re-fetched meta
// carries no entry and no upsert happens; when reconcile lands first, its
// upsert races the DELETE cleanly and the row is deleted afterwards.
//
// This test drives the "RemoveGrant lands first" leg deterministically: it
// seeds meta.grants[uid], runs RemoveGrant (which sweeps meta.grants under
// the lock), then runs reconcile. The stale-snapshot bug would have
// re-inserted the row; the fixed code sees an empty meta inside the lock
// and no-ops.
func TestReconcileMetaGrantsAfterRevokeDoesNotResurrect(t *testing.T) {
	store := memory.New()
	slug := "docRace"
	if err := store.PutMeta(context.Background(), slug, storage.DocMeta{
		Slug:  slug,
		Title: "T",
		Extra: map[string]any{
			storage.CreatorUIDExtraKey: "owner-1",
			storage.GrantsExtraKey: map[string]any{ //nolint:staticcheck // seed reader before revoke
				"reader-9": map[string]any{"role": "reader", "granted_by": "owner-1"},
			},
		},
	}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	svc := service.NewAuthService(store, &config.Config{}, sluglock.NewMemory())
	mirror := &fakeDocMemberMirror{docID: "doc-R", roles: map[string]int{"reader-9": service.DocMemberRoleReader}}
	svc.WithDocMemberMirror(mirror)

	// Admin revokes reader-9. RemoveGrant deletes the doc_member row (mirror
	// side) and sweeps meta.grants under the slug lock.
	if err := svc.RemoveGrant(context.Background(), slug, "reader-9"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, present := mirror.roles["reader-9"]; present {
		t.Fatalf("revoke did not delete doc_member row: roles=%v", mirror.roles)
	}

	// Reconcile now. Enters the same slug lock, re-fetches meta -> no
	// grants entry for reader-9 -> no upsert. Row must stay deleted.
	if err := svc.ReconcileMetaGrantsToDocMember(context.Background(), slug); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if role, present := mirror.roles["reader-9"]; present {
		t.Fatalf("reconcile resurrected revoked reader-9 (role=%d) — TOCTOU bug", role)
	}
	// No upsert call recorded either.
	for _, c := range mirror.calls {
		if c.op == "upsert" && c.uid == "reader-9" {
			t.Fatalf("reconcile issued a stray upsert for revoked reader-9: %+v", c)
		}
	}
}

// Companion: reconcile and revoke run concurrently and always serialize.
// Whichever wins the lock first drains fully before the other observes
// state; the doc_member row is empty at the end because RemoveGrant is
// definitive (it deletes the row regardless of reconcile's order).
func TestReconcileMetaGrantsConcurrentRevokeNoResurrect(t *testing.T) {
	store := memory.New()
	slug := "docRaceConc"
	if err := store.PutMeta(context.Background(), slug, storage.DocMeta{
		Slug:  slug,
		Title: "T",
		Extra: map[string]any{
			storage.CreatorUIDExtraKey: "owner-1",
			storage.GrantsExtraKey: map[string]any{ //nolint:staticcheck // seed reader before concurrent revoke
				"reader-9": map[string]any{"role": "reader", "granted_by": "owner-1"},
			},
		},
	}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	svc := service.NewAuthService(store, &config.Config{}, sluglock.NewMemory())
	mirror := &fakeDocMemberMirror{docID: "doc-RC", roles: map[string]int{"reader-9": service.DocMemberRoleReader}}
	svc.WithDocMemberMirror(mirror)

	done := make(chan error, 2)
	go func() { done <- svc.RemoveGrant(context.Background(), slug, "reader-9") }()
	go func() { done <- svc.ReconcileMetaGrantsToDocMember(context.Background(), slug) }()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent op err: %v", err)
		}
	}
	// Regardless of interleaving, RemoveGrant is definitive: reader-9 must
	// not appear in doc_member at the end.
	if role, present := mirror.roles["reader-9"]; present {
		t.Fatalf("concurrent reconcile resurrected revoked reader-9 (role=%d) — TOCTOU bug", role)
	}
}
