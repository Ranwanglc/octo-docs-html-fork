package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

type deleteRegistrar struct {
	err      error
	deletion docsbackend.Deletion
	calls    int
}

func (*deleteRegistrar) Register(context.Context, docsbackend.Registration, string) (*docsbackend.RegistrationResult, error) {
	return nil, nil
}
func (*deleteRegistrar) Rename(context.Context, string, string, string) {}
func (r *deleteRegistrar) Delete(_ context.Context, d docsbackend.Deletion, _ string) error {
	r.calls++
	r.deletion = d
	return r.err
}

func TestUserDeleteFailurePreservesLocalState(t *testing.T) {
	store := memory.New()
	comments := NewCommentService(store, sluglock.NewMemory())
	registrar := &deleteRegistrar{err: errors.New("backend unavailable")}
	docs := NewDocService(store, store, comments, sluglock.NewMemory(), "", 1<<20).WithDocsBackendRegistration(registrar, nil)
	ctx := context.Background()
	if err := store.PutMeta(ctx, "user-doc", storage.DocMeta{Slug: "user-doc", Extra: map[string]any{
		storage.CreatorUIDExtraKey: "u1", storage.UserPublishExtraKey: true, storage.SpaceIDExtraKey: "space-1",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutDoc(ctx, "user-doc", 1, "<html></html>"); err != nil {
		t.Fatal(err)
	}

	if err := docs.Remove(ctx, "user-doc"); err == nil {
		t.Fatal("delete succeeded despite backend failure")
	}
	if registrar.calls != docsBackendRegisterAttempts {
		t.Fatalf("delete calls = %d, want %d", registrar.calls, docsBackendRegisterAttempts)
	}
	if registrar.deletion.SpaceID != "space-1" || registrar.deletion.Owner != "u1" || !registrar.deletion.UserPublish {
		t.Fatalf("deletion provenance = %+v", registrar.deletion)
	}
	if meta, err := store.GetMeta(ctx, "user-doc"); err != nil || meta == nil {
		t.Fatalf("meta lost after failed backend delete: meta=%+v err=%v", meta, err)
	}
	if _, ok, err := store.GetDoc(ctx, "user-doc", 1); err != nil || !ok {
		t.Fatalf("blob lost after failed backend delete: ok=%v err=%v", ok, err)
	}
}
