package service

import (
	"context"
	"strings"
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

func TestUserPublishRejectsAddingEmptyMountCoordinates(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "user-space", storage.DocMeta{Slug: "user-space", Extra: map[string]any{
		storage.UserPublishExtraKey: true, storage.SpaceIDExtraKey: "space-a", storage.MountTypeExtraKey: "space", storage.CreatorUIDExtraKey: "owner",
	}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		in   PublishInput
	}{
		{name: "group", in: PublishInput{GroupNo: "group-added"}},
		{name: "thread", in: PublishInput{ThreadID: "thread-added"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Slug, tc.in.HTML = "user-space", "<p>changed</p>"
			_, err := docs.Publish(ctx, tc.in)
			requireAppCode(t, err, "mount_conflict")
		})
	}
}

func TestUserDraftRejectsAddingEmptyMountCoordinates(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "user-space-draft", storage.DocMeta{Slug: "user-space-draft", Extra: map[string]any{
		storage.UserPublishExtraKey: true, storage.SpaceIDExtraKey: "space-a", storage.MountTypeExtraKey: "space", storage.CreatorUIDExtraKey: "owner",
	}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		in   PublishInput
	}{
		{name: "group", in: PublishInput{GroupNo: "group-added"}},
		{name: "thread", in: PublishInput{ThreadID: "thread-added"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := docs.SaveDraftWithProvenance(ctx, "user-space-draft", "<p>draft</p>", "", tc.in)
			requireAppCode(t, err, "mount_conflict")
		})
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

func TestPublishRejectsUserClaimOfBlobWithoutMetaBeforeWriting(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	const oldHTML = "<p>legacy</p>"
	if _, err := store.PutDoc(ctx, "blob-residue", 1, oldHTML); err != nil {
		t.Fatal(err)
	}

	_, err := docs.Publish(ctx, PublishInput{
		Slug: "blob-residue", HTML: "<p>user</p>", UserPublish: true, SpaceID: "space-a", CreatorUID: "u1",
	})
	requireAppCode(t, err, "publish_provenance_conflict")
	if got, ok, getErr := store.GetDoc(ctx, "blob-residue", 1); getErr != nil || !ok || got != oldHTML {
		t.Fatalf("legacy blob changed: got=%q ok=%v err=%v", got, ok, getErr)
	}
	if versions, listErr := store.ListVersions(ctx, "blob-residue"); listErr != nil || len(versions) != 1 {
		t.Fatalf("versions after rejection = %v, err=%v", versions, listErr)
	}
	if meta, getErr := store.GetMeta(ctx, "blob-residue"); getErr != nil || meta != nil {
		t.Fatalf("meta written after rejection: meta=%+v err=%v", meta, getErr)
	}
}

func TestSaveDraftRejectsUserClaimOfDraftWithoutMetaBeforeWriting(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	const oldHTML = "<p>legacy draft</p>"
	if _, err := store.PutDraft(ctx, "draft-residue", oldHTML); err != nil {
		t.Fatal(err)
	}

	_, err := docs.SaveDraftWithProvenance(ctx, "draft-residue", "<p>user</p>", "", PublishInput{
		UserPublish: true, SpaceID: "space-a", CreatorUID: "u1",
	})
	requireAppCode(t, err, "publish_provenance_conflict")
	if got, ok, getErr := store.GetDraft(ctx, "draft-residue"); getErr != nil || !ok || got != oldHTML {
		t.Fatalf("legacy draft changed: got=%q ok=%v err=%v", got, ok, getErr)
	}
	if meta, getErr := store.GetMeta(ctx, "draft-residue"); getErr != nil || meta != nil {
		t.Fatalf("meta written after rejection: meta=%+v err=%v", meta, getErr)
	}
}

func TestLegacyGroupMetadataAllowsBotRemount(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "legacy-group", storage.DocMeta{Slug: "legacy-group", Extra: map[string]any{
		storage.GroupNoExtraKey: "group-old",
	}}); err != nil {
		t.Fatal(err)
	}

	_, err := docs.Publish(ctx, PublishInput{
		Slug: "legacy-group", HTML: "<p>new</p>", MountType: "group", GroupNo: "group-new",
	})
	if err != nil {
		t.Fatalf("legacy bot remount: %v", err)
	}
	meta, getErr := store.GetMeta(ctx, "legacy-group")
	if getErr != nil || meta == nil {
		t.Fatalf("meta = %+v, err=%v", meta, getErr)
	}
	_, _, groupNo, _ := meta.PublishProvenance()
	if groupNo != "group-new" {
		t.Fatalf("persisted group = %q, want group-new", groupNo)
	}
	if versions, listErr := store.ListVersions(ctx, "legacy-group"); listErr != nil || len(versions) != 1 {
		t.Fatalf("versions after remount = %v, err=%v", versions, listErr)
	}
}

func TestValidSpaceIDMatchesDocsBackendLimit(t *testing.T) {
	if !ValidSpaceID(strings.Repeat("a", 64)) {
		t.Fatal("64-character space id rejected")
	}
	if ValidSpaceID(strings.Repeat("a", 65)) {
		t.Fatal("65-character space id accepted")
	}
}

func TestLegacyBotCrossMountClearsStaleLocation(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "legacy-thread-move", storage.DocMeta{Slug: "legacy-thread-move", Extra: map[string]any{
		storage.MountTypeExtraKey: "thread",
		storage.ThreadIDExtraKey:  "thread-old",
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := docs.Publish(ctx, PublishInput{
		Slug: "legacy-thread-move", HTML: "<p>new</p>", MountType: "group", GroupNo: "group-new",
	}); err != nil {
		t.Fatalf("legacy bot cross-mount: %v", err)
	}
	meta, err := store.GetMeta(ctx, "legacy-thread-move")
	if err != nil || meta == nil {
		t.Fatalf("meta = %+v, err=%v", meta, err)
	}
	_, _, groupNo, threadID := meta.PublishProvenance()
	if groupNo != "group-new" || threadID != "" {
		t.Fatalf("persisted location: group=%q thread=%q", groupNo, threadID)
	}
}

func TestLegacyExplicitUnmountOverridesInferredLocation(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "legacy-unmount", storage.DocMeta{Slug: "legacy-unmount", Extra: map[string]any{
		storage.GroupNoExtraKey: "group-old",
	}}); err != nil {
		t.Fatal(err)
	}

	result, err := docs.Publish(ctx, PublishInput{
		Slug: "legacy-unmount", HTML: "<p>new</p>", MountTypePresent: true,
	})
	if err != nil {
		t.Fatalf("legacy explicit unmount: %v", err)
	}
	if result.Status != publishStatusUnregistered {
		t.Fatalf("status = %q, want %q", result.Status, publishStatusUnregistered)
	}
	meta, err := store.GetMeta(ctx, "legacy-unmount")
	if err != nil || meta == nil {
		t.Fatalf("meta = %+v, err=%v", meta, err)
	}
	mount, hasMount := meta.MountType()
	_, _, groupNo, threadID := meta.PublishProvenance()
	if !hasMount || mount != "" || groupNo != "" || threadID != "" {
		t.Fatalf("persisted unmount: mount=%q known=%v group=%q thread=%q", mount, hasMount, groupNo, threadID)
	}
}

func TestLegacyMountNormalizationIsPreserved(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "legacy-normalize", storage.DocMeta{Slug: "legacy-normalize", Extra: map[string]any{
		storage.MountTypeExtraKey: " Group ",
		storage.GroupNoExtraKey:   "group-old",
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := docs.Publish(ctx, PublishInput{Slug: "legacy-normalize", HTML: "<p>new</p>"}); err != nil {
		t.Fatalf("legacy normalized republish: %v", err)
	}
	meta, err := store.GetMeta(ctx, "legacy-normalize")
	if err != nil || meta == nil {
		t.Fatalf("meta = %+v, err=%v", meta, err)
	}
	mount, hasMount := meta.MountType()
	_, _, groupNo, _ := meta.PublishProvenance()
	if !hasMount || mount != "group" || groupNo != "group-old" {
		t.Fatalf("normalized provenance: mount=%q known=%v group=%q", mount, hasMount, groupNo)
	}
}

func TestUserProvenanceWithoutCreatorFailsClosed(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "creatorless-user", storage.DocMeta{Slug: "creatorless-user", Extra: map[string]any{
		storage.UserPublishExtraKey: true,
		storage.SpaceIDExtraKey:     "space-a",
		storage.MountTypeExtraKey:   "space",
	}}); err != nil {
		t.Fatal(err)
	}

	_, err := docs.Publish(ctx, PublishInput{
		Slug: "creatorless-user", HTML: "<p>claim</p>", CreatorUID: "attacker",
	})
	requireAppCode(t, err, "publish_provenance_conflict")
	meta, getErr := store.GetMeta(ctx, "creatorless-user")
	if getErr != nil || meta == nil || meta.CreatorUID() != "" {
		t.Fatalf("creatorless provenance mutated: meta=%+v err=%v", meta, getErr)
	}
	if versions, listErr := store.ListVersions(ctx, "creatorless-user"); listErr != nil || len(versions) != 0 {
		t.Fatalf("versions after rejection = %v, err=%v", versions, listErr)
	}
}

func TestSaveDraftWithProvenanceRejectsUserProvenanceWithoutCreator(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "creatorless-user-draft", storage.DocMeta{Slug: "creatorless-user-draft", Extra: map[string]any{
		storage.UserPublishExtraKey: true,
		storage.SpaceIDExtraKey:     "space-a",
		storage.MountTypeExtraKey:   "space",
	}}); err != nil {
		t.Fatal(err)
	}

	_, err := docs.SaveDraftWithProvenance(ctx, "creatorless-user-draft", "<p>claim</p>", "", PublishInput{CreatorUID: "attacker"})
	requireAppCode(t, err, "publish_provenance_conflict")
	requireNoDraft(t, store, "creatorless-user-draft")
}

func TestLegacyDraftExplicitUnmountOverridesInferredLocation(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "legacy-draft-unmount", storage.DocMeta{Slug: "legacy-draft-unmount", Extra: map[string]any{
		storage.GroupNoExtraKey: "group-old",
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := docs.SaveDraftWithProvenance(ctx, "legacy-draft-unmount", "<p>draft</p>", "", PublishInput{MountTypePresent: true}); err != nil {
		t.Fatalf("legacy draft explicit unmount: %v", err)
	}
	meta, err := store.GetMeta(ctx, "legacy-draft-unmount")
	if err != nil || meta == nil {
		t.Fatalf("meta = %+v, err=%v", meta, err)
	}
	mount, hasMount := meta.MountType()
	_, _, groupNo, threadID := meta.PublishProvenance()
	if !hasMount || mount != "" || groupNo != "" || threadID != "" {
		t.Fatalf("draft unmount: mount=%q known=%v group=%q thread=%q", mount, hasMount, groupNo, threadID)
	}
}

func TestNewUserPublishWithoutCreatorIsRejected(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()

	_, err := docs.Publish(ctx, PublishInput{
		Slug: "new-creatorless-user", HTML: "<p>claim</p>", UserPublish: true, SpaceID: "space-a",
	})
	requireAppCode(t, err, "publish_provenance_conflict")
	if meta, getErr := store.GetMeta(ctx, "new-creatorless-user"); getErr != nil || meta != nil {
		t.Fatalf("creatorless user meta written: meta=%+v err=%v", meta, getErr)
	}
}

func TestNewUserDraftWithoutCreatorIsRejected(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()

	_, err := docs.SaveDraftWithProvenance(ctx, "new-creatorless-draft", "<p>claim</p>", "", PublishInput{UserPublish: true, SpaceID: "space-a"})
	requireAppCode(t, err, "publish_provenance_conflict")
	requireNoDraft(t, store, "new-creatorless-draft")
}

func TestLegacyInvalidMountAllowsExplicitRemount(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "legacy-invalid-remount", storage.DocMeta{Slug: "legacy-invalid-remount", Extra: map[string]any{
		storage.MountTypeExtraKey: "broken",
		storage.GroupNoExtraKey:   "group-old",
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := docs.Publish(ctx, PublishInput{
		Slug: "legacy-invalid-remount", HTML: "<p>new</p>", MountType: "thread", ThreadID: "thread-new",
	}); err != nil {
		t.Fatalf("explicit remount should repair invalid legacy mount: %v", err)
	}
	meta, err := store.GetMeta(ctx, "legacy-invalid-remount")
	if err != nil || meta == nil {
		t.Fatalf("meta = %+v, err=%v", meta, err)
	}
	mount, _ := meta.MountType()
	_, _, groupNo, threadID := meta.PublishProvenance()
	if mount != "thread" || groupNo != "" || threadID != "thread-new" {
		t.Fatalf("repaired provenance: mount=%q group=%q thread=%q", mount, groupNo, threadID)
	}
}

func TestLegacyThreadMetadataBackfillsMountAndPreservesThread(t *testing.T) {
	docs, store := draftProvenanceFixture()
	ctx := context.Background()
	if err := store.PutMeta(ctx, "legacy-thread", storage.DocMeta{Slug: "legacy-thread", Extra: map[string]any{
		storage.ThreadIDExtraKey: "thread-old",
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := docs.SaveDraftWithProvenance(ctx, "legacy-thread", "<p>draft</p>", "", PublishInput{}); err != nil {
		t.Fatal(err)
	}
	meta, err := store.GetMeta(ctx, "legacy-thread")
	if err != nil || meta == nil {
		t.Fatalf("meta = %+v, err=%v", meta, err)
	}
	mount, hasMount := meta.MountType()
	_, _, _, threadID := meta.PublishProvenance()
	if !hasMount || mount != "thread" || threadID != "thread-old" {
		t.Fatalf("backfilled provenance: mount=%q hasMount=%v thread=%q", mount, hasMount, threadID)
	}
}
