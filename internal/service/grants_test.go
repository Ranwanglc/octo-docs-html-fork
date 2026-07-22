package service_test

import (
	"context"
	"errors"
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
	f.calls = append(f.calls, mirrorCall{op: "upsert", docID: docID, uid: uid, role: role, grantedBy: grantedBy})
	if f.err != nil {
		return f.err
	}
	if f.roles == nil {
		f.roles = map[string]int{}
	}
	f.roles[uid] = role
	return nil
}

func (f *fakeDocMemberMirror) DeleteGrant(_ context.Context, docID, uid string) error {
	f.calls = append(f.calls, mirrorCall{op: "delete", docID: docID, uid: uid})
	if f.err != nil {
		return f.err
	}
	delete(f.roles, uid)
	return nil
}

func (f *fakeDocMemberMirror) RoleByDocUID(_ context.Context, _, uid string) (int, bool, error) {
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
