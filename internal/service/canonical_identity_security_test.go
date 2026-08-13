package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

type canonicalTestRegistrar struct {
	mu        sync.Mutex
	result    *docsbackend.RegistrationResult
	registers int
	deletes   int
	published int
	renamed   int
}

func (r *canonicalTestRegistrar) Register(context.Context, docsbackend.Registration, string) (*docsbackend.RegistrationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registers++
	return r.result, nil
}
func (r *canonicalTestRegistrar) Rename(context.Context, string, string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renamed++
}
func (*canonicalTestRegistrar) Delete(context.Context, string, string) {}
func (r *canonicalTestRegistrar) DeleteCanonical(context.Context, string, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletes++
	return nil
}
func (r *canonicalTestRegistrar) Published(context.Context, string, string, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published++
	return nil
}

func (r *canonicalTestRegistrar) counts() (registers, deletes, published int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registers, r.deletes, r.published
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
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "doc-mismatch", OctoDocSlug: "doc-mismatch", ShareURL: "https://docs/doc-mismatch", PublisherUID: "other", SpaceID: "space", Created: true}}
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

func TestCanonicalCreateRejectsDraftOnlySlugCollision(t *testing.T) {
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_draft_squat", OctoDocSlug: "d_draft_squat", ShareURL: "https://docs/d_draft_squat", PublisherUID: "publisher", SpaceID: "space", Created: true}}
	docs, store := canonicalTestDocs(t, registrar)
	if _, err := store.PutDraft(context.Background(), "d_draft_squat", "squatted draft"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutMeta(context.Background(), "d_draft_squat", storage.DocMeta{Slug: "d_draft_squat"}); err != nil {
		t.Fatal(err)
	}
	_, err := docs.Publish(context.Background(), canonicalInput())
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Status != http.StatusConflict || appErr.Code != "canonical_identity_conflict" {
		t.Fatalf("error=%v, want 409 canonical identity conflict", err)
	}
	if _, deletes, _ := registrar.counts(); deletes != 1 {
		t.Fatalf("rollback deletes=%d, want 1", deletes)
	}
}

func TestCanonicalDraftCreateRejectsDraftOnlySlugCollision(t *testing.T) {
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_draft_create_squat", OctoDocSlug: "d_draft_create_squat", ShareURL: "https://docs/d_draft_create_squat", PublisherUID: "publisher", SpaceID: "space", Created: true}}
	docs, store := canonicalTestDocs(t, registrar)
	if _, err := store.PutDraft(context.Background(), "d_draft_create_squat", "squatted draft"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutMeta(context.Background(), "d_draft_create_squat", storage.DocMeta{Slug: "d_draft_create_squat"}); err != nil {
		t.Fatal(err)
	}
	_, err := docs.CreateCanonicalDraft(context.Background(), canonicalInput())
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Status != http.StatusConflict || appErr.Code != "canonical_identity_conflict" {
		t.Fatalf("error=%v, want 409 canonical identity conflict", err)
	}
	if _, deletes, _ := registrar.counts(); deletes != 1 {
		t.Fatalf("rollback deletes=%d, want 1", deletes)
	}
}

type failingCanonicalBlobStore struct{ *memory.Store }

func (*failingCanonicalBlobStore) PutDoc(context.Context, string, int, string) (int64, error) {
	return 0, errors.New("simulated post-registration write failure")
}

func TestCanonicalPostRegistrationWriteFailureRollsBackRemoteIdentity(t *testing.T) {
	store := memory.New()
	lock := sluglock.NewMemory()
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_rollback", OctoDocSlug: "d_rollback", ShareURL: "https://docs/d_rollback", PublisherUID: "publisher", SpaceID: "space", Created: true}}
	docs := NewDocService(&failingCanonicalBlobStore{Store: store}, store, NewCommentService(store, lock), lock, "", 1<<20).WithDocsBackendRegistration(registrar, nil)
	if _, err := docs.Publish(context.Background(), canonicalInput()); err == nil {
		t.Fatal("publish succeeded")
	}
	if registrar.deletes != 1 {
		t.Fatalf("rollback deletes=%d, want 1", registrar.deletes)
	}
}

func TestCanonicalReplayWriteFailureDoesNotDeleteExistingIdentity(t *testing.T) {
	store := memory.New()
	lock := sluglock.NewMemory()
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_replay", OctoDocSlug: "d_replay", ShareURL: "https://docs/d_replay", PublisherUID: "publisher", SpaceID: "space"}}
	docs := NewDocService(&failingCanonicalBlobStore{Store: store}, store, NewCommentService(store, lock), lock, "", 1<<20).WithDocsBackendRegistration(registrar, nil)
	if _, err := docs.Publish(context.Background(), canonicalInput()); err == nil {
		t.Fatal("publish succeeded")
	}
	if _, deletes, _ := registrar.counts(); deletes != 0 {
		t.Fatalf("replay rollback deletes=%d, want 0", deletes)
	}
}

type failCanonicalMarkerMetaStore struct {
	*memory.Store
	fail bool
}

func (s *failCanonicalMarkerMetaStore) PutMeta(ctx context.Context, slug string, meta storage.DocMeta) error {
	if s.fail && meta.Extra[storage.CanonicalDocIDExtraKey] != nil {
		s.fail = false
		return errors.New("simulated canonical marker write failure")
	}
	return s.Store.PutMeta(ctx, slug, meta)
}

func TestCanonicalMarkerWriteIsAtomicAndReplaySelfHeals(t *testing.T) {
	store := memory.New()
	meta := &failCanonicalMarkerMetaStore{Store: store, fail: true}
	lock := sluglock.NewMemory()
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_atomic", OctoDocSlug: "d_atomic", ShareURL: "https://docs/d_atomic", PublisherUID: "publisher", SpaceID: "space", Created: true}}
	docs := NewDocService(store, meta, NewCommentService(meta, lock), lock, "", 1<<20).WithDocsBackendRegistration(registrar, nil)
	if _, err := docs.Publish(context.Background(), canonicalInput()); err == nil {
		t.Fatal("publish succeeded despite marker failure")
	}
	if meta, err := store.GetMeta(context.Background(), "d_atomic"); err != nil || meta != nil {
		t.Fatalf("partial identity metadata=%+v err=%v", meta, err)
	}
	if versions, err := store.ListVersions(context.Background(), "d_atomic"); err != nil || len(versions) != 0 {
		t.Fatalf("partial content versions=%v err=%v", versions, err)
	}
	registrar.mu.Lock()
	registrar.result.Created = false
	registrar.mu.Unlock()
	result, err := docs.Publish(context.Background(), canonicalInput())
	if err != nil {
		t.Fatalf("self-heal replay: %v", err)
	}
	if result.Version != 1 || result.DocID != "d_atomic" {
		t.Fatalf("self-heal result=%+v, want canonical v1", result)
	}
}

type probeRegistrar struct {
	mu        sync.Mutex
	registers int
	deletes   int
}

func (r *probeRegistrar) Register(context.Context, docsbackend.Registration, string) (*docsbackend.RegistrationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registers++
	if r.registers == 1 {
		return &docsbackend.RegistrationResult{DocID: "d_probe", OctoDocSlug: "d_probe", ShareURL: "https://docs/d_probe", PublisherUID: "publisher", SpaceID: "space", Created: true}, nil
	}
	if r.deletes > 0 {
		return nil, &docsbackend.CanonicalDocumentDeletedError{}
	}
	return &docsbackend.RegistrationResult{DocID: "d_probe", OctoDocSlug: "d_probe", ShareURL: "https://docs/d_probe", PublisherUID: "publisher", SpaceID: "space", Created: false}, nil
}
func (*probeRegistrar) Rename(context.Context, string, string, string) {}
func (*probeRegistrar) Delete(context.Context, string, string)         {}
func (r *probeRegistrar) DeleteCanonical(context.Context, string, string) error {
	r.mu.Lock()
	r.deletes++
	r.mu.Unlock()
	return nil
}

type gatedFailBlobStore struct {
	*memory.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *gatedFailBlobStore) PutDoc(context.Context, string, int, string) (int64, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return 0, errors.New("first canonical write fails")
}

func TestCanonicalIdempotencyLockPreventsRollbackReplayInterleaving(t *testing.T) {
	store := memory.New()
	lock := sluglock.NewMemory()
	blobs := &gatedFailBlobStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
	registrar := &probeRegistrar{}
	docs := NewDocService(blobs, store, NewCommentService(store, lock), lock, "", 1<<20).WithDocsBackendRegistration(registrar, nil)
	firstDone := make(chan error, 1)
	go func() { _, err := docs.Publish(context.Background(), canonicalInput()); firstDone <- err }()
	<-blobs.entered
	secondDone := make(chan error, 1)
	go func() { _, err := docs.Publish(context.Background(), canonicalInput()); secondDone <- err }()
	close(blobs.release)
	if err := <-firstDone; err == nil {
		t.Fatal("first publish succeeded")
	}
	err := <-secondDone
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Code != "canonical_document_deleted" {
		t.Fatalf("second publish=%v, want deleted-identity conflict after serialized rollback", err)
	}
	registrar.mu.Lock()
	deletes, registers := registrar.deletes, registrar.registers
	registrar.mu.Unlock()
	if deletes != 1 || registers != 2 {
		t.Fatalf("rollback/replay calls deletes=%d registers=%d, want 1/2", deletes, registers)
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

func TestCanonicalRepublishRenamesChangedTitle(t *testing.T) {
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_rename", OctoDocSlug: "d_rename", ShareURL: "https://docs/d_rename", PublisherUID: "publisher", SpaceID: "space", Created: true}}
	docs, _ := canonicalTestDocs(t, registrar)
	in := canonicalInput()
	in.Title = "First"
	if _, err := docs.Publish(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.PublishAuthorized(context.Background(), PublishInput{Slug: "d_rename", HTML: "<p>updated</p>", Title: "Second", PublisherToken: "bot-token"}, nil); err != nil {
		t.Fatal(err)
	}
	registrar.mu.Lock()
	defer registrar.mu.Unlock()
	if registrar.renamed != 1 {
		t.Fatalf("renames=%d, want 1", registrar.renamed)
	}
}

func TestCanonicalDraftReplayAfterPromoteDoesNotResurrectDraft(t *testing.T) {
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_draft", OctoDocSlug: "d_draft", ShareURL: "https://docs/d_draft", PublisherUID: "publisher", SpaceID: "space"}}
	docs, store := canonicalTestDocs(t, registrar)
	in := canonicalInput()
	if _, err := docs.CreateCanonicalDraft(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	result, err := docs.PromoteAuthorizedWithPublisherToken(context.Background(), "d_draft", "", "bot-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.DocID != "d_draft" || result.ShareURL != "https://docs/d_draft" || !result.Registered {
		t.Fatalf("promote result=%+v, want canonical identity", result)
	}
	if registers, _, published := registrar.counts(); registers != 1 || published != 1 {
		t.Fatalf("registers=%d published=%d, want initial registration only and one publication notification", registers, published)
	}
	replay, err := docs.CreateCanonicalDraft(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if replay.DocID != "d_draft" || replay.URL != "https://docs/d_draft" {
		t.Fatalf("draft replay=%+v, want promoted canonical identity", replay)
	}
	if _, exists, err := store.GetDraft(context.Background(), "d_draft"); err != nil || exists {
		t.Fatalf("draft exists=%v err=%v; replay must not resurrect promoted draft", exists, err)
	}
}

func TestCanonicalCreateSameIdempotencyKeyIsSerialized(t *testing.T) {
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_concurrent", OctoDocSlug: "d_concurrent", ShareURL: "https://docs/d_concurrent", PublisherUID: "publisher", SpaceID: "space"}}
	docs, store := canonicalTestDocs(t, registrar)
	start := make(chan struct{})
	results := make(chan *PublishResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := docs.Publish(context.Background(), canonicalInput())
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if result == nil || result.DocID != "d_concurrent" || result.Version != 1 {
			t.Fatalf("concurrent result=%+v", result)
		}
	}
	versions, err := store.ListVersions(context.Background(), "d_concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("versions=%v, want [1]", versions)
	}
	if _, _, published := registrar.counts(); published != 1 {
		t.Fatalf("published notifications=%d, want one first-content signal", published)
	}
}

type gateFirstSlugLock struct {
	sluglock.Locker
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *gateFirstSlugLock) With(ctx context.Context, slug string, fn func() error) error {
	return l.Locker.With(ctx, slug, func() error {
		wait := false
		l.once.Do(func() {
			close(l.entered)
			wait = true
		})
		if wait {
			select {
			case <-l.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return fn()
	})
}

func TestCanonicalCreateRacingLegacyPublishKeepsCanonicalIdentity(t *testing.T) {
	store := memory.New()
	baseLock := sluglock.NewMemory()
	lock := &gateFirstSlugLock{Locker: baseLock, entered: make(chan struct{}), release: make(chan struct{})}
	registrar := &canonicalTestRegistrar{result: &docsbackend.RegistrationResult{DocID: "d_collision", OctoDocSlug: "d_collision", ShareURL: "https://docs/d_collision", PublisherUID: "publisher", SpaceID: "space"}}
	docs := NewDocService(store, store, NewCommentService(store, baseLock), lock, "", 1<<20).WithDocsBackendRegistration(registrar, nil)

	canonicalDone := make(chan error, 1)
	go func() {
		_, err := docs.Publish(context.Background(), canonicalInput())
		canonicalDone <- err
	}()
	<-lock.entered
	legacyDone := make(chan *PublishResult, 1)
	legacyErr := make(chan error, 1)
	go func() {
		result, err := docs.Publish(context.Background(), PublishInput{Slug: "d_collision", HTML: "<p>legacy racing write</p>"})
		legacyDone <- result
		legacyErr <- err
	}()
	close(lock.release)
	if err := <-canonicalDone; err != nil {
		t.Fatalf("canonical publish: %v", err)
	}
	if err := <-legacyErr; err != nil {
		t.Fatalf("legacy publish: %v", err)
	}
	legacy := <-legacyDone
	if legacy.DocID != "d_collision" || !legacy.Registered {
		t.Fatalf("legacy result=%+v, want canonical routing", legacy)
	}
	meta, err := store.GetMeta(context.Background(), "d_collision")
	if err != nil {
		t.Fatal(err)
	}
	if docID, _, ok := meta.CanonicalIdentity(); !ok || docID != "d_collision" {
		t.Fatalf("canonical identity lost after racing legacy publish: %+v", meta)
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
