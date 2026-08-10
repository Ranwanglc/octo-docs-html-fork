package service

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

func draftProvenanceFixture() (*DocService, *memory.Store) {
	store := memory.New()
	locker := sluglock.NewMemory()
	return NewDocService(store, store, NewCommentService(store, locker), locker, "", 1<<20), store
}

func requireAppCode(t *testing.T, err error, code string) {
	t.Helper()
	appErr, ok := err.(*apperr.Error)
	if !ok || appErr.Code != code {
		t.Fatalf("error = %#v, want app error %q", err, code)
	}
}

func requireNoDraft(t *testing.T, store *memory.Store, slug string) {
	t.Helper()
	if _, ok, err := store.GetDraft(context.Background(), slug); err != nil || ok {
		t.Fatalf("draft exists after rejected save: ok=%v err=%v", ok, err)
	}
}

func TestSaveDraftWithProvenanceRejectsInvalidSpaceBeforeBlobWrite(t *testing.T) {
	docs, store := draftProvenanceFixture()
	_, err := docs.SaveDraftWithProvenance(context.Background(), "bad-space", "<p>draft</p>", "", PublishInput{UserPublish: true, SpaceID: "not valid!", CreatorUID: "u1"})
	requireAppCode(t, err, "space_id_invalid")
	requireNoDraft(t, store, "bad-space")
}

func TestSaveDraftWithProvenanceRejectsCrossSpaceBeforeBlobWrite(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "cross-space", storage.DocMeta{Slug: "cross-space", Extra: map[string]any{storage.UserPublishExtraKey: true, storage.SpaceIDExtraKey: "space-a", storage.MountTypeExtraKey: "space", storage.CreatorUIDExtraKey: "owner"}}); err != nil {
		t.Fatal(err)
	}
	_, err := docs.SaveDraftWithProvenance(ctx, "cross-space", "<p>draft</p>", "", PublishInput{UserPublish: true, SpaceID: "space-b", CreatorUID: "other"})
	requireAppCode(t, err, "space_conflict")
	requireNoDraft(t, store, "cross-space")
}

func TestSaveDraftWithProvenanceRejectsBotToUserConversion(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "bot-doc", storage.DocMeta{Slug: "bot-doc", Extra: map[string]any{storage.MountTypeExtraKey: "space", storage.SpaceIDExtraKey: "space-a", storage.CreatorUIDExtraKey: "bot-1"}}); err != nil {
		t.Fatal(err)
	}
	_, err := docs.SaveDraftWithProvenance(ctx, "bot-doc", "<p>draft</p>", "", PublishInput{UserPublish: true, SpaceID: "space-a", CreatorUID: "u1"})
	requireAppCode(t, err, "publish_provenance_conflict")
	requireNoDraft(t, store, "bot-doc")
}

func TestSaveDraftWithProvenanceInheritsExistingUserProvenance(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "user-doc", storage.DocMeta{Slug: "user-doc", Extra: map[string]any{storage.UserPublishExtraKey: true, storage.SpaceIDExtraKey: "space-a", storage.MountTypeExtraKey: "space", storage.CreatorUIDExtraKey: "owner"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.SaveDraftWithProvenance(ctx, "user-doc", "<p>draft</p>", "", PublishInput{CreatorUID: "other"}); err != nil {
		t.Fatal(err)
	}
	meta, err := store.GetMeta(ctx, "user-doc")
	if err != nil || meta == nil {
		t.Fatalf("meta = %+v, err=%v", meta, err)
	}
	userPublish, spaceID, _, _ := meta.PublishProvenance()
	if !userPublish || spaceID != "space-a" || meta.CreatorUID() != "owner" {
		t.Fatalf("provenance overwritten: user=%v space=%q creator=%q", userPublish, spaceID, meta.CreatorUID())
	}
}

func TestSaveDraftWithProvenanceAllowsNewUserAndLegacyBotDrafts(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if _, err := docs.SaveDraftWithProvenance(ctx, "new-user", "<p>user</p>", "", PublishInput{UserPublish: true, SpaceID: "space-a", CreatorUID: "u1"}); err != nil {
		t.Fatalf("new user draft: %v", err)
	}
	if err := store.PutMeta(ctx, "legacy-bot", storage.DocMeta{Slug: "legacy-bot", Extra: map[string]any{storage.CreatorUIDExtraKey: "bot-1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.SaveDraftWithProvenance(ctx, "legacy-bot", "<p>bot</p>", "", PublishInput{CreatorUID: "bot-1"}); err != nil {
		t.Fatalf("legacy bot draft: %v", err)
	}
}
