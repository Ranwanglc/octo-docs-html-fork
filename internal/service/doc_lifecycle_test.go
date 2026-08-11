package service

import (
	"context"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

type deleteRegistrar struct {
	deleted chan string
}

func (*deleteRegistrar) Register(context.Context, docsbackend.Registration, string) (*docsbackend.RegistrationResult, error) {
	return nil, nil
}
func (*deleteRegistrar) Rename(context.Context, string, string, string) {}
func (r *deleteRegistrar) Delete(_ context.Context, slug, _ string) {
	r.deleted <- slug
}

func TestUserDocumentRemoveIsRejectedUntilBackendDeleteIsSupported(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	registrar := &deleteRegistrar{deleted: make(chan string, 1)}
	docs := NewDocService(store, store, NewCommentService(store, locker), locker, "", 1<<20).
		WithDocsBackendRegistration(registrar, nil)
	ctx := context.Background()
	if err := store.PutMeta(ctx, "user-doc", storage.DocMeta{Slug: "user-doc", Extra: map[string]any{
		storage.CreatorUIDExtraKey: "u1", storage.UserPublishExtraKey: true, storage.SpaceIDExtraKey: "space-1",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutDoc(ctx, "user-doc", 1, "<html></html>"); err != nil {
		t.Fatal(err)
	}

	err := docs.Remove(ctx, "user-doc")
	requireAppCode(t, err, "user_publish_delete_unsupported")
	select {
	case slug := <-registrar.deleted:
		t.Fatalf("unexpected backend delete for %q", slug)
	case <-time.After(50 * time.Millisecond):
	}
	if meta, err := store.GetMeta(ctx, "user-doc"); err != nil || meta == nil {
		t.Fatalf("local metadata removed: meta=%+v err=%v", meta, err)
	}
	if _, ok, err := store.GetDoc(ctx, "user-doc", 1); err != nil || !ok {
		t.Fatalf("local document removed: ok=%v err=%v", ok, err)
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
