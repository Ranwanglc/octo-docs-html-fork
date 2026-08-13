package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

type canonicalTestRegistrar struct {
	result    *docsbackend.RegistrationResult
	deletes   int
	published int
}

func (r *canonicalTestRegistrar) Register(context.Context, docsbackend.Registration, string) (*docsbackend.RegistrationResult, error) {
	return r.result, nil
}
func (*canonicalTestRegistrar) Rename(context.Context, string, string, string) {}
func (*canonicalTestRegistrar) Delete(context.Context, string, string)         {}
func (r *canonicalTestRegistrar) DeleteCanonical(context.Context, string, string) error {
	r.deletes++
	return nil
}
func (r *canonicalTestRegistrar) Published(context.Context, string, string, string) error {
	r.published++
	return nil
}

func canonicalTestDocs(t *testing.T, registrar *canonicalTestRegistrar) (*DocService, *memory.Store) {
	t.Helper()
	store := memory.New()
	lock := sluglock.NewMemory()
	return NewDocService(store, store, NewCommentService(store, lock), lock, "", 1<<20).WithDocsBackendRegistration(registrar, nil), store
}

func canonicalInput() PublishInput {
	return PublishInput{HTML: "<p>canonical</p>", IdempotencyKey: "create-key", PublisherToken: "bot-token", PublisherUID: "publisher", PublisherSpaceID: "space"}
}

func TestCanonicalCreateRejectsPublisherMismatchAndRollsBack(t *testing.T) {
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "doc-mismatch", OctoDocSlug: "doc-mismatch", ShareURL: "https://docs/doc-mismatch", PublisherUID: "other", SpaceID: "space"}}
	docs, store := canonicalTestDocs(t, registrar)
	_, err := docs.Publish(context.Background(), canonicalInput())
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Status != http.StatusForbidden || appErr.Code != "registration_identity_mismatch" {
		t.Fatalf("error=%v, want 403 identity mismatch", err)
	}
	if registrar.deletes != 1 {
		t.Fatalf("rollback deletes=%d, want 1", registrar.deletes)
	}
	if meta, _ := store.GetMeta(context.Background(), "doc-mismatch"); meta != nil {
		t.Fatalf("unexpected local metadata: %+v", meta)
	}
}

func TestCanonicalReplayRejectsLegacySlugCollision(t *testing.T) {
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_legacy", OctoDocSlug: "d_legacy", ShareURL: "https://docs/d_legacy", PublisherUID: "publisher", SpaceID: "space"}}
	docs, store := canonicalTestDocs(t, registrar)
	if _, err := store.PutDoc(context.Background(), "d_legacy", 1, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutMeta(context.Background(), "d_legacy", storage.DocMeta{Slug: "d_legacy", Versions: []storage.VersionRef{{N: 1}}}); err != nil {
		t.Fatal(err)
	}
	_, err := docs.Publish(context.Background(), canonicalInput())
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Status != http.StatusConflict || appErr.Code != "canonical_identity_conflict" {
		t.Fatalf("error=%v, want 409 canonical identity conflict", err)
	}
}

type failingCanonicalBlobStore struct{ *memory.Store }

func (*failingCanonicalBlobStore) PutDoc(context.Context, string, int, string) (int64, error) {
	return 0, errors.New("simulated post-registration write failure")
}

func TestCanonicalPostRegistrationWriteFailureRollsBackRemoteIdentity(t *testing.T) {
	store := memory.New()
	lock := sluglock.NewMemory()
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_rollback", OctoDocSlug: "d_rollback", ShareURL: "https://docs/d_rollback", PublisherUID: "publisher", SpaceID: "space"}}
	docs := NewDocService(&failingCanonicalBlobStore{Store: store}, store, NewCommentService(store, lock), lock, "", 1<<20).WithDocsBackendRegistration(registrar, nil)
	if _, err := docs.Publish(context.Background(), canonicalInput()); err == nil {
		t.Fatal("publish succeeded")
	}
	if registrar.deletes != 1 {
		t.Fatalf("rollback deletes=%d, want 1", registrar.deletes)
	}
}

func TestCanonicalMarkedSlugRepublishesWithoutReregistration(t *testing.T) {
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_republish", OctoDocSlug: "d_republish", ShareURL: "https://docs/d_republish", PublisherUID: "publisher", SpaceID: "space"}}
	docs, _ := canonicalTestDocs(t, registrar)
	if _, err := docs.Publish(context.Background(), canonicalInput()); err != nil {
		t.Fatal(err)
	}
	result, err := docs.PublishAuthorized(context.Background(), PublishInput{Slug: "d_republish", HTML: "<p>updated</p>", PublisherToken: "bot-token"}, func(exists bool) error {
		if !exists {
			t.Fatal("canonical document was not found for update authorization")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if registrar.published != 1 {
		t.Fatalf("published notifications=%d, want 1", registrar.published)
	}
	if result.Version != 2 || !result.Registered || result.DocID != "d_republish" {
		t.Fatalf("republish result=%+v", result)
	}
}

func TestCanonicalDraftReplayAfterPromoteDoesNotResurrectDraft(t *testing.T) {
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_draft", OctoDocSlug: "d_draft", ShareURL: "https://docs/d_draft", PublisherUID: "publisher", SpaceID: "space"}}
	docs, store := canonicalTestDocs(t, registrar)
	in := canonicalInput()
	if _, err := docs.CreateCanonicalDraft(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.PromoteAuthorized(context.Background(), "d_draft", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.CreateCanonicalDraft(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.GetDraft(context.Background(), "d_draft"); err != nil || exists {
		t.Fatalf("draft exists=%v err=%v; replay must not resurrect promoted draft", exists, err)
	}
}

type failOnceDeleteStore struct {
	*memory.Store
	fail bool
}

func (s *failOnceDeleteStore) DeleteDoc(context.Context, string) error {
	if s.fail {
		s.fail = false
		return errors.New("local delete failed")
	}
	return s.Store.DeleteDoc(context.Background(), "d_delete")
}

func TestCanonicalDeleteRetriesAfterPartialLocalFailure(t *testing.T) {
	store := memory.New()
	blobs := &failOnceDeleteStore{Store: store, fail: true}
	lock := sluglock.NewMemory()
	registrar := &canonicalTestRegistrar{}
	docs := NewDocService(blobs, store, NewCommentService(store, lock), lock, "", 1<<20).WithDocsBackendRegistration(registrar, nil)
	ctx := context.Background()
	if _, err := store.PutDoc(ctx, "d_delete", 1, "doc"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutMeta(ctx, "d_delete", storage.DocMeta{Slug: "d_delete", Versions: []storage.VersionRef{{N: 1}}, Extra: map[string]any{storage.CanonicalDocIDExtraKey: "d_delete", storage.CanonicalShareURLExtraKey: "https://docs/d_delete"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.RemoveCanonical(ctx, "d_delete", "bot-token"); err == nil {
		t.Fatal("first delete succeeded")
	}
	if _, err := docs.RemoveCanonical(ctx, "d_delete", "bot-token"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if registrar.deletes != 2 {
		t.Fatalf("remote deletes=%d, want retry", registrar.deletes)
	}
	if meta, _ := store.GetMeta(ctx, "d_delete"); meta != nil {
		t.Fatalf("metadata remains: %+v", meta)
	}
}
