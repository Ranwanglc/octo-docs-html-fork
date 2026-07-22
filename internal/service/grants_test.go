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
}

func (f *fakeDocMemberMirror) DocIDBySlug(_ context.Context, slug string) (string, bool, error) {
	if f.resolveErr != nil {
		return "", false, f.resolveErr
	}
	if f.docID == "" {
		return "node-" + slug, true, nil
	}
	return f.docID, true, nil
}

func (f *fakeDocMemberMirror) UpsertDirectGrant(_ context.Context, docID, uid string, role int, grantedBy string) error {
	f.calls = append(f.calls, mirrorCall{op: "upsert", docID: docID, uid: uid, role: role, grantedBy: grantedBy})
	return f.err
}

func (f *fakeDocMemberMirror) DeleteGrant(_ context.Context, docID, uid string) error {
	f.calls = append(f.calls, mirrorCall{op: "delete", docID: docID, uid: uid})
	return f.err
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
	if got := nilMeta.GrantRole("u"); got != "" {
		t.Fatalf("nil meta GrantRole = %q; want empty", got)
	}
	m := &storage.DocMeta{Extra: map[string]any{
		storage.GrantsExtraKey: map[string]any{
			"friend": map[string]any{"role": "reader"},
		},
	}}
	if got := m.GrantRole("friend"); got != "reader" {
		t.Fatalf("GrantRole(friend) = %q; want reader", got)
	}
	if got := m.GrantRole("stranger"); got != "" {
		t.Fatalf("GrantRole(stranger) = %q; want empty", got)
	}
	if got := m.GrantRole(""); got != "" {
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

func TestGrantMirrorErrorsDoNotBlock(t *testing.T) {
	svc, slug := newGrantSvc(t)
	svc.WithDocMemberMirror(&fakeDocMemberMirror{err: errors.New("mirror down")})
	ctx := context.Background()

	if err := svc.AddGrant(ctx, slug, "u1", "reader", "owner"); err != nil {
		t.Fatalf("AddGrant with mirror error: %v", err)
	}
	if err := svc.RemoveGrant(ctx, slug, "u1"); err != nil {
		t.Fatalf("RemoveGrant with mirror error: %v", err)
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
