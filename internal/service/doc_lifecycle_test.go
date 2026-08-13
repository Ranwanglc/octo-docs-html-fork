package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

type deleteRegistrar struct {
	deleted    chan string
	registerID string
}

func (r *deleteRegistrar) Register(_ context.Context, reg docsbackend.Registration, _ string) (*docsbackend.RegistrationResult, error) {
	if r.registerID == "" {
		return nil, nil
	}
	return &docsbackend.RegistrationResult{
		DocID: r.registerID, OctoDocSlug: reg.OctoDocSlug, ShareURL: "https://docs.example/" + r.registerID,
	}, nil
}
func (*deleteRegistrar) Rename(context.Context, string, string, string) {}
func (r *deleteRegistrar) Delete(_ context.Context, slug, _ string) {
	r.deleted <- slug
}

func TestRegisteredUserDocumentRemoveIsRejectedUntilBackendCascadeExists(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	registrar := &deleteRegistrar{deleted: make(chan string, 1), registerID: "doc-user"}
	docs := NewDocService(store, store, NewCommentService(store, locker), locker, "", 1<<20).
		WithDocsBackendRegistration(registrar, nil)
	ctx := context.Background()
	result, err := docs.Publish(ctx, PublishInput{
		Slug: "user-doc", HTML: "<html></html>", UserPublish: true, SpaceID: "space-1", CreatorUID: "u1",
	})
	if err != nil || !result.Registered {
		t.Fatalf("publish result=%+v err=%v", result, err)
	}

	err = docs.RemoveAuthorized(ctx, "user-doc", func(_ context.Context, provenance PublishProvenance) error {
		if !provenance.UserPublish || provenance.CreatorUID != "u1" || provenance.SpaceID != "space-1" {
			t.Fatalf("provenance = %+v", provenance)
		}
		return nil
	})
	requireAppCode(t, err, "user_publish_delete_via_backend")
	select {
	case slug := <-registrar.deleted:
		t.Fatalf("unexpected backend delete for %q", slug)
	case <-time.After(50 * time.Millisecond):
	}
	if meta, err := store.GetMeta(ctx, "user-doc"); err != nil || meta == nil {
		t.Fatalf("local metadata removed: meta=%+v err=%v", meta, err)
	}
}

func TestUnregisteredUserDocumentRemoveRequiresProvenanceAuthorization(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	registrar := &deleteRegistrar{deleted: make(chan string, 1)}
	docs := NewDocService(store, store, NewCommentService(store, locker), locker, "", 1<<20).
		WithDocsBackendRegistration(registrar, nil)
	ctx := context.Background()
	if err := store.PutMeta(ctx, "local-user-doc", storage.DocMeta{Slug: "local-user-doc", Extra: map[string]any{
		storage.CreatorUIDExtraKey: "u1", storage.UserPublishExtraKey: true, storage.SpaceIDExtraKey: "space-1",
		storage.DocsBackendRegistrationStateKey: storage.DocsBackendRegistrationLocalOnly,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutDoc(ctx, "local-user-doc", 1, "<html></html>"); err != nil {
		t.Fatal(err)
	}

	denied := errors.New("denied")
	if err := docs.RemoveAuthorized(ctx, "local-user-doc", func(context.Context, PublishProvenance) error { return denied }); !errors.Is(err, denied) {
		t.Fatalf("denied remove error = %v", err)
	}
	if meta, err := store.GetMeta(ctx, "local-user-doc"); err != nil || meta == nil {
		t.Fatalf("denied remove changed meta: meta=%+v err=%v", meta, err)
	}

	if err := docs.RemoveAuthorized(ctx, "local-user-doc", func(_ context.Context, provenance PublishProvenance) error {
		if !provenance.UserPublish || provenance.CreatorUID != "u1" || provenance.SpaceID != "space-1" {
			t.Fatalf("provenance = %+v", provenance)
		}
		return nil
	}); err != nil {
		t.Fatalf("authorized local cleanup: %v", err)
	}
	if meta, err := store.GetMeta(ctx, "local-user-doc"); err != nil || meta != nil {
		t.Fatalf("local metadata not removed: meta=%+v err=%v", meta, err)
	}
	select {
	case slug := <-registrar.deleted:
		t.Fatalf("unexpected backend delete for %q", slug)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDraftOnlyUserDocumentIsMarkedLocalOnly(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	docs := NewDocService(store, store, NewCommentService(store, locker), locker, "", 1<<20)
	ctx := context.Background()
	if _, err := docs.SaveDraftWithProvenance(ctx, "draft-local", "<html></html>", "", PublishInput{
		UserPublish: true, SpaceID: "space-1", CreatorUID: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	meta, err := store.GetMeta(ctx, "draft-local")
	if err != nil || meta == nil {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	state, docID, version := meta.DocsBackendRegistration()
	if state != storage.DocsBackendRegistrationLocalOnly || docID != "" || version != 0 {
		t.Fatalf("registration state=%q docID=%q version=%d", state, docID, version)
	}
}

func TestFailedUserRegistrationRemainsUnconfirmedForDelete(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	registrar := &deleteRegistrar{deleted: make(chan string, 1)}
	docs := NewDocService(store, store, NewCommentService(store, locker), locker, "", 1<<20).
		WithDocsBackendRegistration(registrar, nil)
	ctx := context.Background()
	result, err := docs.Publish(ctx, PublishInput{
		Slug: "registration-failed", HTML: "<html></html>", UserPublish: true, SpaceID: "space-1", CreatorUID: "u1",
	})
	if err != nil || result.Status != publishStatusRegisterFailed {
		t.Fatalf("publish result=%+v err=%v", result, err)
	}
	meta, err := store.GetMeta(ctx, "registration-failed")
	if err != nil || meta == nil {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	state, docID, version := meta.DocsBackendRegistration()
	if state != storage.DocsBackendRegistrationPending || docID != "" || version != result.Version {
		t.Fatalf("registration state=%q docID=%q version=%d", state, docID, version)
	}
	err = docs.RemoveAuthorized(ctx, "registration-failed", func(context.Context, PublishProvenance) error { return nil })
	requireAppCode(t, err, "user_publish_delete_via_backend")
}

func TestFailedRepublishPreservesConfirmedBackendDocID(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	registrar := &deleteRegistrar{deleted: make(chan string, 1), registerID: "doc-confirmed"}
	docs := NewDocService(store, store, NewCommentService(store, locker), locker, "", 1<<20).
		WithDocsBackendRegistration(registrar, nil)
	ctx := context.Background()
	first, err := docs.Publish(ctx, PublishInput{
		Slug: "registered-republish", HTML: "<html>v1</html>", UserPublish: true, SpaceID: "space-1", CreatorUID: "u1",
	})
	if err != nil || !first.Registered {
		t.Fatalf("first publish result=%+v err=%v", first, err)
	}
	registrar.registerID = ""
	second, err := docs.Publish(ctx, PublishInput{
		Slug: "registered-republish", HTML: "<html>v2</html>", UserPublish: true, SpaceID: "space-1", CreatorUID: "u1",
	})
	if err != nil || second.Status != publishStatusRegisterFailed {
		t.Fatalf("second publish result=%+v err=%v", second, err)
	}
	meta, err := store.GetMeta(ctx, "registered-republish")
	if err != nil || meta == nil {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	state, docID, version := meta.DocsBackendRegistration()
	if state != storage.DocsBackendRegistrationPending || docID != "doc-confirmed" || version != second.Version {
		t.Fatalf("registration state=%q docID=%q version=%d", state, docID, version)
	}
	err = docs.RemoveAuthorized(ctx, "registered-republish", func(context.Context, PublishProvenance) error { return nil })
	requireAppCode(t, err, "user_publish_delete_via_backend")
}

func TestLegacyPublishedUserDraftDoesNotBecomeLocalOnly(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	docs := NewDocService(store, store, NewCommentService(store, locker), locker, "", 1<<20)
	ctx := context.Background()
	if err := store.PutMeta(ctx, "legacy-user", storage.DocMeta{Slug: "legacy-user", Versions: []storage.VersionRef{{N: 1}}, Extra: map[string]any{
		storage.CreatorUIDExtraKey: "u1", storage.UserPublishExtraKey: true, storage.SpaceIDExtraKey: "space-1", storage.MountTypeExtraKey: "space",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.SaveDraftWithProvenance(ctx, "legacy-user", "<html></html>", "", PublishInput{CreatorUID: "u1"}); err != nil {
		t.Fatal(err)
	}
	meta, err := store.GetMeta(ctx, "legacy-user")
	if err != nil || meta == nil {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	state, docID, version := meta.DocsBackendRegistration()
	if state != "" || docID != "" || version != 0 {
		t.Fatalf("historical registration state changed: %q %q %d", state, docID, version)
	}
	err = docs.RemoveAuthorized(ctx, "legacy-user", func(context.Context, PublishProvenance) error { return nil })
	requireAppCode(t, err, "user_publish_delete_via_backend")
}

func TestLegacyDraftWithoutRegistrationStateRemainsFailClosed(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	docs := NewDocService(store, store, NewCommentService(store, locker), locker, "", 1<<20)
	ctx := context.Background()
	if err := store.PutMeta(ctx, "legacy-draft", storage.DocMeta{Slug: "legacy-draft", Extra: map[string]any{
		storage.CreatorUIDExtraKey: "u1", storage.UserPublishExtraKey: true, storage.SpaceIDExtraKey: "space-1", storage.MountTypeExtraKey: "space",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.SaveDraftWithProvenance(ctx, "legacy-draft", "<html></html>", "", PublishInput{CreatorUID: "u1"}); err != nil {
		t.Fatal(err)
	}
	meta, err := store.GetMeta(ctx, "legacy-draft")
	if err != nil || meta == nil {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	state, docID, version := meta.DocsBackendRegistration()
	if state != "" || docID != "" || version != 0 {
		t.Fatalf("historical registration state changed: %q %q %d", state, docID, version)
	}
	err = docs.RemoveAuthorized(ctx, "legacy-draft", func(context.Context, PublishProvenance) error { return nil })
	requireAppCode(t, err, "user_publish_delete_via_backend")
}

func TestStaleRegistrationResultCannotOverwriteNewerPendingVersion(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	docs := NewDocService(store, store, NewCommentService(store, locker), locker, "", 1<<20)
	ctx := context.Background()
	if err := store.PutMeta(ctx, "racing-user", storage.DocMeta{Slug: "racing-user", Title: "new", Versions: []storage.VersionRef{{N: 1}, {N: 2}}, Extra: map[string]any{
		storage.CreatorUIDExtraKey: "u1", storage.UserPublishExtraKey: true, storage.SpaceIDExtraKey: "space-1",
		storage.DocsBackendRegistrationStateKey: storage.DocsBackendRegistrationPending, storage.DocsBackendRegistrationVersionKey: 2,
	}}); err != nil {
		t.Fatal(err)
	}
	if docs.setDocsBackendRegistrationState(ctx, "racing-user", storage.DocsBackendRegistrationRegistered, "doc-old", 1) {
		t.Fatal("stale registration result updated metadata")
	}
	meta, err := store.GetMeta(ctx, "racing-user")
	if err != nil || meta == nil || len(meta.Versions) != 2 || meta.Title != "new" {
		t.Fatalf("metadata changed: meta=%+v err=%v", meta, err)
	}
	state, docID, version := meta.DocsBackendRegistration()
	if state != storage.DocsBackendRegistrationPending || docID != "" || version != 2 {
		t.Fatalf("registration state=%q docID=%q version=%d", state, docID, version)
	}
}

func TestBotDocumentRemoveStillUsesBotDeleteBySlug(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	registrar := &deleteRegistrar{deleted: make(chan string, 1)}
	docs := NewDocService(store, store, NewCommentService(store, locker), locker, "", 1<<20).
		WithDocsBackendRegistration(registrar, nil)
	ctx := context.Background()
	if err := store.PutMeta(ctx, "bot-doc", storage.DocMeta{Slug: "bot-doc", Extra: map[string]any{
		storage.CreatorUIDExtraKey: "bot-1",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutDoc(ctx, "bot-doc", 1, "<html></html>"); err != nil {
		t.Fatal(err)
	}

	if err := docs.Remove(ctx, "bot-doc"); err != nil {
		t.Fatal(err)
	}
	select {
	case slug := <-registrar.deleted:
		if slug != "bot-doc" {
			t.Fatalf("deleted slug = %q", slug)
		}
	case <-time.After(time.Second):
		t.Fatal("bot delete was not called")
	}
}

// stubTitleResolver is a TitleResolver fake driving the Render/GetDraft title
// pull tests: hit (returns title), miss (ok=false), empty title, and error.
type stubTitleResolver struct {
	title string
	ok    bool
	err   error
	// lastSpace records the spaceID the resolver was called with so tests can
	// assert the call is scoped from meta provenance.
	lastSpace string
	calls     int
}

func (r *stubTitleResolver) resolve(_ context.Context, _, spaceID string) (string, bool, error) {
	r.calls++
	r.lastSpace = spaceID
	return r.title, r.ok, r.err
}

func newTitleResolverFixture(t *testing.T, resolver TitleResolver) (*DocService, context.Context, string) {
	t.Helper()
	store := memory.New()
	locker := sluglock.NewMemory()
	docs := NewDocService(store, store, NewCommentService(store, locker), locker, "", 1<<20).
		WithTitleResolver(resolver)
	ctx := context.Background()
	slug := "title-doc"
	if _, err := store.PutDoc(ctx, slug, 1, "<html><body>v1</body></html>"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutDraft(ctx, slug, "<html><body>draft</body></html>"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutMeta(ctx, slug, storage.DocMeta{
		Slug: slug, Title: "Local Title", Versions: []storage.VersionRef{{N: 1}},
		Extra: map[string]any{
			storage.UserPublishExtraKey: true, storage.SpaceIDExtraKey: "space-9",
		},
	}); err != nil {
		t.Fatal(err)
	}
	return docs, ctx, slug
}

// Render pulls the display title from the resolver (docs-backend is the single
// source of truth) and passes the slug's persisted space_id to it.
func TestRenderTitleResolverOverridesLocalTitle(t *testing.T) {
	resolver := &stubTitleResolver{title: "MySQL Title", ok: true}
	docs, ctx, slug := newTitleResolverFixture(t, resolver.resolve)
	rd, err := docs.Render(ctx, slug, 1)
	if err != nil || rd == nil {
		t.Fatalf("render = %+v, %v", rd, err)
	}
	if rd.Title != "MySQL Title" {
		t.Fatalf("render title = %q; want resolver title", rd.Title)
	}
	if resolver.lastSpace != "space-9" {
		t.Fatalf("resolver called with space %q; want space-9 from meta provenance", resolver.lastSpace)
	}
}

// GetDraft applies the same resolver pull.
func TestGetDraftTitleResolverOverridesLocalTitle(t *testing.T) {
	resolver := &stubTitleResolver{title: "Draft Backend Title", ok: true}
	docs, ctx, slug := newTitleResolverFixture(t, resolver.resolve)
	rd, err := docs.GetDraft(ctx, slug)
	if err != nil || rd == nil {
		t.Fatalf("get draft = %+v, %v", rd, err)
	}
	if rd.Title != "Draft Backend Title" {
		t.Fatalf("draft title = %q; want resolver title", rd.Title)
	}
}

// Resolver errors are display-only: the local title is kept and the render
// still succeeds.
func TestRenderTitleResolverErrorKeepsLocalTitle(t *testing.T) {
	resolver := &stubTitleResolver{err: errors.New("mysql down")}
	docs, ctx, slug := newTitleResolverFixture(t, resolver.resolve)
	rd, err := docs.Render(ctx, slug, 1)
	if err != nil || rd == nil {
		t.Fatalf("render = %+v, %v; resolver error must not fail a render", rd, err)
	}
	if rd.Title != "Local Title" {
		t.Fatalf("render title = %q; want local title kept on resolver error", rd.Title)
	}
}

// Resolver miss (ok=false) or empty title keeps the local title.
func TestRenderTitleResolverMissKeepsLocalTitle(t *testing.T) {
	for name, resolver := range map[string]*stubTitleResolver{
		"miss":      {ok: false},
		"empty hit": {title: "", ok: true},
	} {
		t.Run(name, func(t *testing.T) {
			docs, ctx, slug := newTitleResolverFixture(t, resolver.resolve)
			rd, err := docs.Render(ctx, slug, 1)
			if err != nil || rd == nil {
				t.Fatalf("render = %+v, %v", rd, err)
			}
			if rd.Title != "Local Title" {
				t.Fatalf("render title = %q; want local title on resolver miss", rd.Title)
			}
		})
	}
}

// Unwired resolver (nil, e.g. non-MySQL drivers) leaves Render/GetDraft
// behavior exactly as before: local title only, no resolver call.
func TestRenderWithoutTitleResolverUsesLocalTitle(t *testing.T) {
	docs, ctx, slug := newTitleResolverFixture(t, nil)
	rd, err := docs.Render(ctx, slug, 1)
	if err != nil || rd == nil || rd.Title != "Local Title" {
		t.Fatalf("render = %+v, %v; want local title without resolver", rd, err)
	}
	draft, err := docs.GetDraft(ctx, slug)
	if err != nil || draft == nil || draft.Title != "Local Title" {
		t.Fatalf("get draft = %+v, %v; want local title without resolver", draft, err)
	}
}
