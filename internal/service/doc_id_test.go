package service_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/core"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

type canonicalRegistrar struct {
	result    *docsbackend.RegistrationResult
	err       error
	seen      []docsbackend.Registration
	tokens    []string
	deletes   []struct{ ref, token string }
	deleteErr error
	delegated []docsbackend.DelegatedDelete
	secrets   []string
}

func (r *canonicalRegistrar) Register(_ context.Context, reg docsbackend.Registration, token string) (*docsbackend.RegistrationResult, error) {
	r.seen = append(r.seen, reg)
	r.tokens = append(r.tokens, token)
	return r.result, r.err
}
func (*canonicalRegistrar) Rename(context.Context, string, string, string) {}
func (r *canonicalRegistrar) Delete(_ context.Context, ref, token string) error {
	r.deletes = append(r.deletes, struct{ ref, token string }{ref, token})
	return r.deleteErr
}
func (r *canonicalRegistrar) DeleteDelegated(_ context.Context, in docsbackend.DelegatedDelete, secret string) error {
	r.delegated = append(r.delegated, in)
	r.secrets = append(r.secrets, secret)
	return r.deleteErr
}

type failFirstDeleteDocStore struct {
	*memory.Store
	failed bool
}

func (s *failFirstDeleteDocStore) DeleteDoc(ctx context.Context, slug string) error {
	if !s.failed {
		s.failed = true
		return errors.New("simulated local blob delete failure")
	}
	return s.Store.DeleteDoc(ctx, slug)
}

type failFirstPutCommentsStore struct {
	*memory.Store
	failed bool
}

func (s *failFirstPutCommentsStore) PutComments(ctx context.Context, slug string, comments []core.Comment) error {
	if !s.failed {
		s.failed = true
		return errors.New("simulated comment merge failure")
	}
	return s.Store.PutComments(ctx, slug, comments)
}

type retryRegistrar struct {
	mu     sync.Mutex
	calls  int
	second chan struct{}
}

func (r *retryRegistrar) Register(context.Context, docsbackend.Registration, string) (*docsbackend.RegistrationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls == 2 {
		close(r.second)
	}
	return &docsbackend.RegistrationResult{DocID: "doc-concurrent", OctoDocSlug: "doc-concurrent", ShareURL: "u", Created: r.calls == 1}, nil
}
func (*retryRegistrar) Rename(context.Context, string, string, string) {}
func (*retryRegistrar) Delete(context.Context, string, string) error   { return nil }

type gatedLocker struct {
	inner   sluglock.Locker
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (l *gatedLocker) With(ctx context.Context, key string, fn func() error) error {
	return l.inner.With(ctx, key, func() error {
		first := false
		l.once.Do(func() { first = true; close(l.entered) })
		if first {
			select {
			case <-l.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return fn()
	})
}

func canonicalDocs(t *testing.T, registrar *canonicalRegistrar) (*service.DocService, *memory.Store) {
	t.Helper()
	store := memory.New()
	lock := sluglock.NewMemory()
	docs := service.NewDocService(store, store, service.NewCommentService(store, lock), lock, "https://octo.test", 5<<20).
		WithDocsBackendRegistration(registrar, nil)
	return docs, store
}

func TestDeletedCanonicalKeyDoesNotRecreatePublishOrDraft(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*service.DocService) error
	}{
		{name: "publish", run: func(d *service.DocService) error {
			_, err := d.Publish(context.Background(), service.PublishInput{HTML: "resurrect", IdempotencyKey: "deleted-key", PublisherToken: "bot"})
			return err
		}},
		{name: "draft", run: func(d *service.DocService) error {
			_, err := d.SaveDraftMounted(context.Background(), service.PublishInput{HTML: "resurrect", IdempotencyKey: "deleted-key", PublisherToken: "bot"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				http.Error(w, `{"error":"canonical_document_deleted"}`, http.StatusGone)
			}))
			defer ts.Close()
			store := memory.New()
			lock := sluglock.NewMemory()
			docs := service.NewDocService(store, store, service.NewCommentService(store, lock), lock, "", 5<<20).
				WithDocsBackendRegistration(docsbackend.New(ts.URL, "", nil), nil)
			err := tc.run(docs)
			if err == nil {
				t.Fatal("create retry succeeded")
			}
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Status != http.StatusConflict || appErr.Code != "canonical_document_deleted" {
				t.Fatalf("error=%v; want 409 canonical_document_deleted", err)
			}
			if calls != 1 {
				t.Fatalf("registration calls=%d, want terminal failure without retries", calls)
			}
			if metas, _ := store.ListMeta(context.Background()); len(metas) != 0 {
				t.Fatalf("local metadata resurrected: %+v", metas)
			}
		})
	}
}

func TestCanonicalCreateRegistersBeforeAnyWriteRegardlessOfMount(t *testing.T) {
	for _, mount := range []string{"", "group"} {
		t.Run("mount="+mount, func(t *testing.T) {
			registrar := &canonicalRegistrar{result: &docsbackend.RegistrationResult{DocID: "doc-42", OctoDocSlug: "doc-42", ShareURL: "https://docs.test/doc-42", Created: true}}
			docs, store := canonicalDocs(t, registrar)
			result, err := docs.Publish(context.Background(), service.PublishInput{
				HTML: "<html>x</html>", Title: "Title", MountType: mount,
				IdempotencyKey: "create-123", PublisherToken: "bot-token",
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Slug != "doc-42" || result.DocID != "doc-42" {
				t.Fatalf("result=%+v", result)
			}
			if len(registrar.seen) != 1 || registrar.seen[0].IdempotencyKey != "create-123" || registrar.seen[0].MountType != mount {
				t.Fatalf("registration=%+v", registrar.seen)
			}
			if versions, _ := store.ListVersions(context.Background(), "doc-42"); len(versions) != 1 {
				t.Fatalf("versions=%v", versions)
			}
		})
	}
}

func TestCanonicalCreateRejectsDocRefAndRequiresKeyAndBot(t *testing.T) {
	registrar := &canonicalRegistrar{result: &docsbackend.RegistrationResult{DocID: "doc-42", OctoDocSlug: "doc-42", ShareURL: "u"}}
	docs, _ := canonicalDocs(t, registrar)
	base := service.PublishInput{HTML: "<html>x</html>", IdempotencyKey: "key", PublisherToken: "bot"}
	for name, mutate := range map[string]func(*service.PublishInput){
		"ref": func(in *service.PublishInput) { in.Slug = "friendly" },
		"key": func(in *service.PublishInput) { in.IdempotencyKey = "" },
		"bot": func(in *service.PublishInput) { in.PublisherToken = "" },
	} {
		t.Run(name, func(t *testing.T) {
			in := base
			mutate(&in)
			if _, err := docs.Publish(context.Background(), in); err == nil {
				t.Fatal("create succeeded")
			}
		})
	}
	if len(registrar.seen) != 0 {
		t.Fatalf("registrations=%d", len(registrar.seen))
	}
}

func TestCanonicalRegistrationFailureWritesNothing(t *testing.T) {
	registrar := &canonicalRegistrar{err: errors.New("down")}
	docs, store := canonicalDocs(t, registrar)
	_, err := docs.Publish(context.Background(), service.PublishInput{HTML: "<html>x</html>", IdempotencyKey: "key", PublisherToken: "bot"})
	if err == nil {
		t.Fatal("publish succeeded")
	}
	if meta, _ := store.GetMeta(context.Background(), "doc-42"); meta != nil {
		t.Fatalf("meta=%v", meta)
	}
}

func TestExistingRefNeverRegistersAndLegacyStaysKeyed(t *testing.T) {
	registrar := &canonicalRegistrar{result: &docsbackend.RegistrationResult{DocID: "other", OctoDocSlug: "other", ShareURL: "u"}}
	docs, store := canonicalDocs(t, registrar)
	ctx := context.Background()
	_, _ = store.PutDoc(ctx, "legacy", 1, "old")
	if err := store.PutMeta(ctx, "legacy", storage.DocMeta{Slug: "legacy", Versions: []storage.VersionRef{{N: 1}}}); err != nil {
		t.Fatal(err)
	}
	seenAuth := false
	res, err := docs.PublishAuthorized(ctx, service.PublishInput{Slug: "legacy", HTML: "new", PublisherToken: "bot"}, func(ref string, exists bool) error {
		seenAuth = ref == "legacy" && exists
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seenAuth || res.Slug != "legacy" || len(registrar.seen) != 0 {
		t.Fatalf("auth=%v result=%+v registrations=%d", seenAuth, res, len(registrar.seen))
	}
	if versions, _ := store.ListVersions(ctx, "legacy"); len(versions) != 2 {
		t.Fatalf("versions=%v", versions)
	}
}

func TestUnknownRefDoesNotCreateWhenRegistrarEnabled(t *testing.T) {
	registrar := &canonicalRegistrar{}
	docs, _ := canonicalDocs(t, registrar)
	if _, err := docs.PublishAuthorized(context.Background(), service.PublishInput{Slug: "unknown", HTML: "x"}, func(string, bool) error { return nil }); err == nil {
		t.Fatal("unknown legacy ref created")
	}
}

func TestConcurrentCanonicalPublishRetryWaitsAndReturnsV1(t *testing.T) {
	store := memory.New()
	registrar := &retryRegistrar{second: make(chan struct{})}
	locker := &gatedLocker{inner: sluglock.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	docs := service.NewDocService(store, store, service.NewCommentService(store, locker), locker, "", 5<<20).
		WithDocsBackendRegistration(registrar, nil)
	input := func(html string) service.PublishInput {
		return service.PublishInput{HTML: html, IdempotencyKey: "same", PublisherToken: "bot"}
	}
	results := make(chan *service.PublishResult, 2)
	errs := make(chan error, 2)
	go func() { r, err := docs.Publish(context.Background(), input("one")); results <- r; errs <- err }()
	<-locker.entered
	go func() { r, err := docs.Publish(context.Background(), input("two")); results <- r; errs <- err }()
	<-registrar.second
	select {
	case <-results:
		t.Fatal("retry returned before creator completed its locked write")
	case <-time.After(25 * time.Millisecond):
	}
	close(locker.release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if result := <-results; result.Version != 1 {
			t.Fatalf("result=%+v", result)
		}
	}
	versions, _ := store.ListVersions(context.Background(), "doc-concurrent")
	if len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("versions=%v", versions)
	}
	html, _, _ := store.GetDoc(context.Background(), "doc-concurrent", 1)
	if html == "two" {
		t.Fatal("retry overwrote creator content")
	}
}

func TestConcurrentCanonicalDraftRetryWaitsAndDoesNotOverwrite(t *testing.T) {
	store := memory.New()
	registrar := &retryRegistrar{second: make(chan struct{})}
	locker := &gatedLocker{inner: sluglock.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	docs := service.NewDocService(store, store, service.NewCommentService(store, locker), locker, "", 5<<20).
		WithDocsBackendRegistration(registrar, nil)
	input := func(html string) service.PublishInput {
		return service.PublishInput{HTML: html, IdempotencyKey: "same", PublisherToken: "bot"}
	}
	results := make(chan *service.DraftResult, 2)
	errs := make(chan error, 2)
	go func() { r, err := docs.SaveDraftMounted(context.Background(), input("one")); results <- r; errs <- err }()
	<-locker.entered
	go func() { r, err := docs.SaveDraftMounted(context.Background(), input("two")); results <- r; errs <- err }()
	<-registrar.second
	select {
	case <-results:
		t.Fatal("retry returned before creator completed its locked draft write")
	case <-time.After(25 * time.Millisecond):
	}
	close(locker.release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if result := <-results; result.DocID != "doc-concurrent" {
			t.Fatalf("result=%+v", result)
		}
	}
	html, ok, _ := store.GetDraft(context.Background(), "doc-concurrent")
	if !ok || html == "two" {
		t.Fatalf("draft=%q ok=%v", html, ok)
	}
}

func TestCanonicalRetryDoesNotCreateSecondVersion(t *testing.T) {
	registrar := &canonicalRegistrar{result: &docsbackend.RegistrationResult{DocID: "doc-42", OctoDocSlug: "doc-42", ShareURL: "u", Created: true}}
	docs, store := canonicalDocs(t, registrar)
	ctx := context.Background()
	if _, err := docs.Publish(ctx, service.PublishInput{HTML: "one", IdempotencyKey: "same", PublisherToken: "bot"}); err != nil {
		t.Fatal(err)
	}
	registrar.result.Created = false
	res, err := docs.Publish(ctx, service.PublishInput{HTML: "two", IdempotencyKey: "same", PublisherToken: "bot"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 1 {
		t.Fatalf("retry version=%d", res.Version)
	}
	if versions, _ := store.ListVersions(ctx, "doc-42"); len(versions) != 1 {
		t.Fatalf("versions=%v", versions)
	}
}

func TestCanonicalRetryCompletesFailedCommentMergeExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := &failFirstPutCommentsStore{Store: memory.New()}
	lock := sluglock.NewMemory()
	registrar := &canonicalRegistrar{result: &docsbackend.RegistrationResult{
		DocID: "doc-comment-retry", OctoDocSlug: "doc-comment-retry", ShareURL: "u", Created: true,
	}}
	docs := service.NewDocService(store, store, service.NewCommentService(store, lock), lock, "", 5<<20).
		WithDocsBackendRegistration(registrar, nil)
	text, version := "local note", 1
	local := []core.Comment{{ID: "local-1", Text: &text, Version: &version}}
	anchorText := "existing note"
	existing := []core.Comment{{
		ID: "existing-1", Text: &anchorText, Version: &version,
		Anchor: &core.Anchor{Kind: "element", AID: "missing", Selector: `[data-odoc-aid="missing"]`},
	}}
	if err := store.Store.PutComments(ctx, "doc-comment-retry", existing); err != nil {
		t.Fatal(err)
	}

	if _, err := docs.Publish(ctx, service.PublishInput{
		HTML: "<section>original</section>", LocalComments: local,
		IdempotencyKey: "same", PublisherToken: "bot",
	}); err == nil {
		t.Fatal("initial publish succeeded despite comment merge failure")
	}
	if versions, _ := store.ListVersions(ctx, "doc-comment-retry"); len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("half-state versions=%v, want persisted v1", versions)
	}
	if meta, _ := store.GetMeta(ctx, "doc-comment-retry"); meta == nil || len(meta.Versions) != 1 {
		t.Fatalf("half-state meta=%+v, want persisted v1", meta)
	}
	if comments, _ := store.GetComments(ctx, "doc-comment-retry"); len(comments) != 1 || comments[0].ID != "existing-1" || comments[0].Anchor == nil || comments[0].Anchor.Kind != "element" {
		t.Fatalf("half-state comments=%+v, want original unreconciled comment only", comments)
	}

	registrar.result.Created = false
	retry := service.PublishInput{
		HTML: "<section>must not overwrite</section>", LocalComments: local,
		IdempotencyKey: "same", PublisherToken: "bot",
	}
	for attempt := 1; attempt <= 2; attempt++ {
		res, err := docs.Publish(ctx, retry)
		if err != nil {
			t.Fatalf("retry %d: %v", attempt, err)
		}
		if res.Version != 1 {
			t.Fatalf("retry %d result=%+v, want v1", attempt, res)
		}
	}
	versions, _ := store.ListVersions(ctx, "doc-comment-retry")
	if len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("versions after retries=%v, want only v1", versions)
	}
	html, _, _ := store.GetDoc(ctx, "doc-comment-retry", 1)
	if !strings.Contains(html, "original") || strings.Contains(html, "must not overwrite") {
		t.Fatalf("v1 content overwritten on retry: %q", html)
	}
	comments, _ := store.GetComments(ctx, "doc-comment-retry")
	if len(comments) != 2 || comments[0].ID != "existing-1" || comments[1].ID != "local-1" {
		t.Fatalf("comments after repeated retry=%+v, want existing plus one local comment", comments)
	}
	snaps := core.SnapshotList(comments, 1)
	if len(snaps) != 2 || snaps[0].Anchor == nil || snaps[0].Anchor.AID == "missing" {
		t.Fatalf("comments were not reconciled exactly once: %+v", snaps)
	}
	anchorChanges := 0
	for _, event := range comments[0].Events {
		if event.Kind == "anchor_changed" {
			anchorChanges++
		}
	}
	if anchorChanges != 1 {
		t.Fatalf("repeated retry duplicated reconciliation events: %+v", comments[0].Events)
	}
}

func TestCanonicalDeleteFailsClosedOnBackendError(t *testing.T) {
	registrar := &canonicalRegistrar{deleteErr: errors.New("backend down")}
	docs, store := canonicalDocs(t, registrar)
	ctx := context.Background()
	_, _ = store.PutDoc(ctx, "doc-err", 1, "x")
	_ = store.PutMeta(ctx, "doc-err", storage.DocMeta{Slug: "doc-err", Versions: []storage.VersionRef{{N: 1}}, Extra: map[string]any{storage.CanonicalDocIDExtraKey: "doc-err"}})
	if err := docs.RemoveAuthorized(ctx, "doc-err", service.DeleteAuth{PublisherToken: "bot"}); err == nil {
		t.Fatal("delete succeeded")
	}
	if versions, _ := store.ListVersions(ctx, "doc-err"); len(versions) != 1 {
		t.Fatalf("local deleted: %v", versions)
	}
}

func TestCanonicalDeleteWithoutRegistrarFailsClosedAndRetainsLocalData(t *testing.T) {
	store := memory.New()
	lock := sluglock.NewMemory()
	docs := service.NewDocService(store, store, service.NewCommentService(store, lock), lock, "", 5<<20)
	ctx := context.Background()
	_, _ = store.PutDoc(ctx, "doc-local", 1, "x")
	_ = store.PutMeta(ctx, "doc-local", storage.DocMeta{Slug: "doc-local", Versions: []storage.VersionRef{{N: 1}}, Extra: map[string]any{
		storage.CanonicalDocIDExtraKey: "doc-local", storage.CanonicalShareURLExtraKey: "https://share/doc-local",
	}})
	if err := docs.RemoveAuthorized(ctx, "doc-local", service.DeleteAuth{PublisherToken: "bot"}); err == nil {
		t.Fatal("canonical delete succeeded without registrar")
	}
	if versions, _ := store.ListVersions(ctx, "doc-local"); len(versions) != 1 {
		t.Fatalf("local data deleted: %v", versions)
	}
	if meta, _ := store.GetMeta(ctx, "doc-local"); meta == nil {
		t.Fatal("local metadata deleted")
	}
}

func TestCanonicalDeleteRetryAfterRemoteSuccessFinishesLocalCleanup(t *testing.T) {
	registrar := &canonicalRegistrar{}
	store := &failFirstDeleteDocStore{Store: memory.New()}
	lock := sluglock.NewMemory()
	docs := service.NewDocService(store, store.Store, service.NewCommentService(store.Store, lock), lock, "", 5<<20).
		WithDocsBackendRegistration(registrar, nil)
	ctx := context.Background()
	_, _ = store.PutDoc(ctx, "doc-retry", 1, "x")
	_ = store.PutMeta(ctx, "doc-retry", storage.DocMeta{Slug: "doc-retry", Versions: []storage.VersionRef{{N: 1}}, Extra: map[string]any{storage.CanonicalDocIDExtraKey: "doc-retry"}})

	if err := docs.RemoveAuthorized(ctx, "doc-retry", service.DeleteAuth{PublisherToken: "bot-token"}); err == nil {
		t.Fatal("first delete succeeded despite local failure")
	}
	if err := docs.RemoveAuthorized(ctx, "doc-retry", service.DeleteAuth{PublisherToken: "bot-token"}); err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	if len(registrar.deletes) != 2 {
		t.Fatalf("remote deletes=%d, want retry call", len(registrar.deletes))
	}
	if versions, _ := store.ListVersions(ctx, "doc-retry"); len(versions) != 0 {
		t.Fatalf("local versions remain: %v", versions)
	}
}

func TestCanonicalDeleteUsesRequestCredentialAndMissingIsNotFound(t *testing.T) {
	registrar := &canonicalRegistrar{}
	docs, store := canonicalDocs(t, registrar)
	ctx := context.Background()
	if err := docs.RemoveAuthorized(ctx, "missing", service.DeleteAuth{PublisherToken: "bot-token"}); err == nil {
		t.Fatal("missing delete succeeded")
	}
	_, _ = store.PutDoc(ctx, "doc-42", 1, "x")
	if err := store.PutMeta(ctx, "doc-42", storage.DocMeta{Slug: "doc-42", Versions: []storage.VersionRef{{N: 1}}, Extra: map[string]any{storage.CanonicalDocIDExtraKey: "doc-42"}}); err != nil {
		t.Fatal(err)
	}
	if err := docs.RemoveAuthorized(ctx, "doc-42", service.DeleteAuth{PublisherToken: "bot-token"}); err != nil {
		t.Fatal(err)
	}
	if len(registrar.deletes) != 1 || registrar.deletes[0].ref != "doc-42" || registrar.deletes[0].token != "bot-token" {
		t.Fatalf("deletes=%+v", registrar.deletes)
	}
}

func TestHumanDeleteDelegatesCanonicalAndLegacyWithoutBotFallback(t *testing.T) {
	registrar := &canonicalRegistrar{}
	docs, store := canonicalDocs(t, registrar)
	ctx := context.Background()
	for _, tc := range []struct {
		slug      string
		canonical bool
	}{{"doc-human", true}, {"legacy-human", false}} {
		_, _ = store.PutDoc(ctx, tc.slug, 1, "x")
		var extra map[string]any
		if tc.canonical {
			extra = map[string]any{storage.CanonicalDocIDExtraKey: tc.slug}
		}
		_ = store.PutMeta(ctx, tc.slug, storage.DocMeta{Slug: tc.slug, Versions: []storage.VersionRef{{N: 1}}, Extra: extra})
		if err := docs.RemoveAuthorized(ctx, tc.slug, service.DeleteAuth{ActorUID: "human-admin", DelegationSecret: "shared-secret", SuperAdmin: true}); err != nil {
			t.Fatal(err)
		}
	}
	if len(registrar.deletes) != 0 || len(registrar.delegated) != 2 {
		t.Fatalf("bot=%+v delegated=%+v", registrar.deletes, registrar.delegated)
	}
	if registrar.delegated[0].DocID != "doc-human" || registrar.delegated[1].DocID != "" || registrar.delegated[1].ActorUID != "human-admin" {
		t.Fatalf("delegated=%+v", registrar.delegated)
	}
}

func TestHumanDeleteEmptySecretFailsClosedAndRetainsLocalData(t *testing.T) {
	registrar := &canonicalRegistrar{}
	docs, store := canonicalDocs(t, registrar)
	ctx := context.Background()
	_, _ = store.PutDoc(ctx, "closed", 1, "x")
	_ = store.PutMeta(ctx, "closed", storage.DocMeta{Slug: "closed", Versions: []storage.VersionRef{{N: 1}}})
	if err := docs.RemoveAuthorized(ctx, "closed", service.DeleteAuth{ActorUID: "human"}); err == nil {
		t.Fatal("delete succeeded without delegation secret")
	}
	if versions, _ := store.ListVersions(ctx, "closed"); len(versions) != 1 {
		t.Fatalf("local data deleted: %v", versions)
	}
	if len(registrar.deletes) != 0 || len(registrar.delegated) != 0 {
		t.Fatal("remote delete attempted")
	}
}
