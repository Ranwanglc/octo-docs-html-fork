package service_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

type blockerRegistrar struct {
	mu          sync.Mutex
	calls       int
	publishErr  error
	publishErrs []error
	published   []struct{ ref, title, token string }
	deletes     []string
	deleteErr   error
}

func (r *blockerRegistrar) Register(context.Context, docsbackend.Registration, string) (*docsbackend.RegistrationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return &docsbackend.RegistrationResult{DocID: "doc-guard", OctoDocSlug: "doc-guard", ShareURL: "https://share/doc-guard", Created: r.calls == 1}, nil
}
func (*blockerRegistrar) Rename(context.Context, string, string, string) {}
func (r *blockerRegistrar) Delete(_ context.Context, ref, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletes = append(r.deletes, ref)
	return r.deleteErr
}
func (r *blockerRegistrar) Published(_ context.Context, ref, title, token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published = append(r.published, struct{ ref, title, token string }{ref, title, token})
	if len(r.publishErrs) > 0 {
		err := r.publishErrs[0]
		r.publishErrs = r.publishErrs[1:]
		return err
	}
	return r.publishErr
}

type failOnceMetaStore struct {
	*memory.Store
	fail bool
}

func (s *failOnceMetaStore) PutMeta(ctx context.Context, slug string, meta storage.DocMeta) error {
	if s.fail {
		s.fail = false
		return errors.New("metadata unavailable")
	}
	return s.Store.PutMeta(ctx, slug, meta)
}

type recordingLocker struct {
	inner sluglock.Locker
	mu    sync.Mutex
	keys  []string
}

func (l *recordingLocker) With(ctx context.Context, key string, fn func() error) error {
	l.mu.Lock()
	l.keys = append(l.keys, key)
	l.mu.Unlock()
	return l.inner.With(ctx, key, fn)
}

type recordingLockStore struct {
	*memory.Store
	locker sluglock.Locker
}

func (s *recordingLockStore) Locker() sluglock.Locker { return s.locker }

func blockerService(store *memory.Store, registrar *blockerRegistrar) *service.DocService {
	lock := sluglock.NewMemory()
	return service.NewDocService(store, store, service.NewCommentService(store, lock), lock, "", 5<<20).WithDocsBackendRegistration(registrar, nil)
}

func TestCanonicalInitializationUsesSharedStoreGuardAcrossServices(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(map[bool]string{false: "publish", true: "draft"}[draft], func(t *testing.T) {
			store := memory.New()
			reg := &blockerRegistrar{}
			d1, d2 := blockerService(store, reg), blockerService(store, reg)
			run := func(d *service.DocService, body string) error {
				in := service.PublishInput{HTML: body, IdempotencyKey: "same", PublisherToken: "bot"}
				if draft {
					_, e := d.SaveDraftMounted(context.Background(), in)
					return e
				}
				_, e := d.Publish(context.Background(), in)
				return e
			}
			start := make(chan struct{})
			errs := make(chan error, 2)
			go func() { <-start; errs <- run(d1, "first") }()
			go func() { <-start; errs <- run(d2, "second") }()
			close(start)
			if e := <-errs; e != nil {
				t.Fatal(e)
			}
			if e := <-errs; e != nil {
				t.Fatal(e)
			}
			if draft {
				body, _, _ := store.GetDraft(context.Background(), "doc-guard")
				if body != "first" && body != "second" {
					t.Fatalf("draft=%q", body)
				}
			} else if vs, _ := store.ListVersions(context.Background(), "doc-guard"); len(vs) != 1 {
				t.Fatalf("versions=%v", vs)
			}
		})
	}
}

func TestCanonicalCreateUsesOneSharedDocIDLock(t *testing.T) {
	base := memory.New()
	locker := &recordingLocker{inner: sluglock.NewMemory()}
	store := &recordingLockStore{Store: base, locker: locker}
	reg := &blockerRegistrar{}
	local := sluglock.NewMemory()
	docs := service.NewDocService(store, store, service.NewCommentService(store, local), local, "", 5<<20).WithDocsBackendRegistration(reg, nil)
	if _, err := docs.Publish(context.Background(), service.PublishInput{HTML: "x", IdempotencyKey: "key", PublisherToken: "bot"}); err != nil {
		t.Fatal(err)
	}
	locker.mu.Lock()
	defer locker.mu.Unlock()
	if len(locker.keys) != 1 || locker.keys[0] != "doc-guard" {
		t.Fatalf("shared lock keys=%v, want one docID lock", locker.keys)
	}
}

func TestCanonicalInitializationRecoversBlobWithoutMetadata(t *testing.T) {
	base := memory.New()
	store := &failOnceMetaStore{Store: base, fail: true}
	reg := &blockerRegistrar{}
	lock := sluglock.NewMemory()
	docs := service.NewDocService(store, store, service.NewCommentService(store, lock), lock, "", 5<<20).WithDocsBackendRegistration(reg, nil)
	in := service.PublishInput{HTML: "<html><body><h1>first</h1></body></html>", Title: "Recovered", CreatorUID: "owner", IdempotencyKey: "same", PublisherToken: "bot"}
	if res, err := docs.Publish(context.Background(), in); err == nil || res != nil {
		t.Fatalf("first publish = %+v, %v; want metadata failure", res, err)
	}
	if versions, _ := base.ListVersions(context.Background(), "doc-guard"); len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("half-state versions = %v; want [1]", versions)
	}
	in.HTML = "<html><body><h1>must not overwrite</h1></body></html>"
	res, err := docs.Publish(context.Background(), in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.Version != 1 || res.DocID != "doc-guard" || !res.Registered {
		t.Fatalf("retry result = %+v", res)
	}
	meta, _ := base.GetMeta(context.Background(), "doc-guard")
	docID, shareURL, canonical := meta.CanonicalIdentity()
	if meta == nil || len(meta.Versions) != 1 || meta.Versions[0].N != 1 || meta.CreatorUID() != "owner" || !canonical || docID != "doc-guard" || shareURL != "https://share/doc-guard" {
		t.Fatalf("recovered meta = %+v", meta)
	}
	html, _, _ := base.GetDoc(context.Background(), "doc-guard", 1)
	if strings.Contains(html, "must not overwrite") || !strings.Contains(html, "first") {
		t.Fatalf("persisted html was overwritten: %q", html)
	}
}

func TestCanonicalDraftInitializationRecoversMetadataWithoutOverwritingDraft(t *testing.T) {
	base := memory.New()
	store := &failOnceMetaStore{Store: base, fail: true}
	reg := &blockerRegistrar{}
	lock := sluglock.NewMemory()
	docs := service.NewDocService(store, store, service.NewCommentService(store, lock), lock, "", 5<<20).WithDocsBackendRegistration(reg, nil)
	in := service.PublishInput{HTML: "<html><body><h1>first draft</h1></body></html>", Title: "Recovered draft", CreatorUID: "owner", IdempotencyKey: "same", PublisherToken: "bot"}
	if res, err := docs.SaveDraftMounted(context.Background(), in); err == nil || res != nil {
		t.Fatalf("first save = %+v, %v; want metadata failure", res, err)
	}
	in.HTML = "<html><body><h1>must not overwrite</h1></body></html>"
	res, err := docs.SaveDraftMounted(context.Background(), in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.DocID != "doc-guard" {
		t.Fatalf("retry result = %+v", res)
	}
	meta, _ := base.GetMeta(context.Background(), "doc-guard")
	docID, shareURL, canonical := meta.CanonicalIdentity()
	if meta == nil || meta.CreatorUID() != "owner" || !canonical || docID != "doc-guard" || shareURL != "https://share/doc-guard" {
		t.Fatalf("recovered meta = %+v", meta)
	}
	if _, ok := meta.Extra[storage.DraftExtraKey]; !ok {
		t.Fatalf("draft marker missing: %+v", meta)
	}
	html, ok, _ := base.GetDraft(context.Background(), "doc-guard")
	if !ok || strings.Contains(html, "must not overwrite") || !strings.Contains(html, "first draft") {
		t.Fatalf("persisted draft was overwritten: %q", html)
	}
}

func TestPublishedNotificationRetriesTransientFailure(t *testing.T) {
	store := memory.New()
	reg := &blockerRegistrar{publishErrs: []error{errors.New("temporary"), nil}}
	docs := blockerService(store, reg)
	ctx := context.Background()
	_, _ = store.PutDoc(ctx, "doc-guard", 1, "old")
	_ = store.PutMeta(ctx, "doc-guard", storage.DocMeta{Slug: "doc-guard", Versions: []storage.VersionRef{{N: 1}}, Extra: map[string]any{storage.CanonicalDocIDExtraKey: "doc-guard"}})
	res, err := docs.Publish(ctx, service.PublishInput{Slug: "doc-guard", HTML: "new", PublisherToken: "actual-bot"})
	if err != nil || res.Version != 2 {
		t.Fatalf("publish = %+v, %v", res, err)
	}
	if len(reg.published) != 2 || reg.published[1].token != "actual-bot" {
		t.Fatalf("published attempts = %+v", reg.published)
	}
	if versions, _ := store.ListVersions(ctx, "doc-guard"); len(versions) != 2 {
		t.Fatalf("versions = %v; retry must not mint another version", versions)
	}
}

func TestReplaceElementNotifiesWithRequestBotToken(t *testing.T) {
	store := memory.New()
	reg := &blockerRegistrar{}
	docs := blockerService(store, reg)
	ctx := context.Background()
	first, err := docs.Publish(ctx, service.PublishInput{
		HTML: "<html><body><section>old</section></body></html>", IdempotencyKey: "replace-test", PublisherToken: "create-bot",
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := docs.Render(ctx, "doc-guard", first.Version)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(rendered.HTML, `data-odoc-aid="`)
	if start < 0 {
		t.Fatalf("no aid in %q", rendered.HTML)
	}
	start += len(`data-odoc-aid="`)
	end := strings.Index(rendered.HTML[start:], `"`)
	res, err := docs.ReplaceElementAuthorized(ctx, "doc-guard", first.Version, rendered.HTML[start:start+end], "<figure>new</figure>", "replace-bot")
	if err != nil || res.Version != 2 {
		t.Fatalf("replace = %+v, %v", res, err)
	}
	if len(reg.published) != 2 || reg.published[1].token != "replace-bot" || reg.published[1].ref != "doc-guard" {
		t.Fatalf("published = %+v", reg.published)
	}
}

func TestCanonicalRepublishAndPromoteRestoreIdentityAndNotify(t *testing.T) {
	store := memory.New()
	reg := &blockerRegistrar{}
	docs := blockerService(store, reg)
	ctx := context.Background()
	_, _ = store.PutDoc(ctx, "doc-guard", 1, "old")
	_ = store.PutMeta(ctx, "doc-guard", storage.DocMeta{Slug: "doc-guard", Title: "Old", Versions: []storage.VersionRef{{N: 1}}, Extra: map[string]any{storage.CanonicalDocIDExtraKey: "doc-guard", storage.CanonicalShareURLExtraKey: "https://share/doc-guard"}})
	res, err := docs.Publish(ctx, service.PublishInput{Slug: "doc-guard", HTML: "new", Title: "New", PublisherToken: "bot"})
	if err != nil {
		t.Fatal(err)
	}
	if res.DocID != "doc-guard" || !res.Registered || res.URL != "https://share/doc-guard" {
		t.Fatalf("republish=%+v", res)
	}
	if len(reg.published) != 1 || reg.published[0].title != "New" {
		t.Fatalf("notifications=%+v", reg.published)
	}
	if _, err = docs.SaveDraft(ctx, "doc-guard", "draft", "Draft", ""); err != nil {
		t.Fatal(err)
	}
	promoted, err := docs.PromoteAuthorized(ctx, "doc-guard", "Promoted", "bot")
	if err != nil {
		t.Fatal(err)
	}
	if promoted.DocID != "doc-guard" || !promoted.Registered || promoted.URL != "https://share/doc-guard" {
		t.Fatalf("promote=%+v", promoted)
	}
	if len(reg.published) != 2 || reg.published[1].title != "Promoted" || reg.published[1].token != "bot" {
		t.Fatalf("notifications=%+v", reg.published)
	}
}

func TestNotificationFailureDoesNotRollbackOrDuplicate(t *testing.T) {
	store := memory.New()
	reg := &blockerRegistrar{publishErr: errors.New("down")}
	docs := blockerService(store, reg)
	ctx := context.Background()
	_, _ = store.PutDoc(ctx, "doc-guard", 1, "old")
	_ = store.PutMeta(ctx, "doc-guard", storage.DocMeta{Slug: "doc-guard", Title: "T", Versions: []storage.VersionRef{{N: 1}}, Extra: map[string]any{storage.CanonicalDocIDExtraKey: "doc-guard"}})
	res, err := docs.Publish(ctx, service.PublishInput{Slug: "doc-guard", HTML: "new", PublisherToken: "bot"})
	if err != nil || res.Version != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	vs, _ := store.ListVersions(ctx, "doc-guard")
	if len(vs) != 2 {
		t.Fatalf("versions=%v", vs)
	}
	reg.publishErr = nil
	res, err = docs.Publish(ctx, service.PublishInput{Slug: "doc-guard", HTML: "newer", PublisherToken: "bot"})
	if err != nil || res.Version != 3 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(reg.published) != 4 {
		t.Fatalf("notifications=%d; want 3 bounded failed attempts plus one later success", len(reg.published))
	}
}

func TestLegacyDeleteCallsRemoteBeforeLocal(t *testing.T) {
	store := memory.New()
	reg := &blockerRegistrar{deleteErr: errors.New("down")}
	docs := blockerService(store, reg)
	ctx := context.Background()
	_, _ = store.PutDoc(ctx, "legacy", 1, "x")
	_ = store.PutMeta(ctx, "legacy", storage.DocMeta{Slug: "legacy", Versions: []storage.VersionRef{{N: 1}}})
	if err := docs.RemoveAuthorized(ctx, "legacy", service.DeleteAuth{PublisherToken: "bot"}); err == nil {
		t.Fatal("delete succeeded")
	}
	if vs, _ := store.ListVersions(ctx, "legacy"); len(vs) != 1 {
		t.Fatal("local content deleted")
	}
	if len(reg.deletes) != 1 {
		t.Fatalf("remote deletes=%d", len(reg.deletes))
	}
}

func TestDelegatedDelete404RetainsLocalData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer server.Close()
	store := memory.New()
	lock := sluglock.NewMemory()
	client := docsbackend.New(server.URL+"/v1/bot/docs", "", nil)
	docs := service.NewDocService(store, store, service.NewCommentService(store, lock), lock, "", 5<<20).WithDocsBackendRegistration(client, nil)
	ctx := context.Background()
	_, _ = store.PutDoc(ctx, "canonical", 1, "x")
	_ = store.PutMeta(ctx, "canonical", storage.DocMeta{Slug: "canonical", Versions: []storage.VersionRef{{N: 1}}, Extra: map[string]any{storage.CanonicalDocIDExtraKey: "canonical"}})
	err := docs.RemoveAuthorized(ctx, "canonical", service.DeleteAuth{ActorUID: "human"})
	if err == nil {
		t.Fatal("human delete unexpectedly succeeded")
	}
	if versions, _ := store.ListVersions(ctx, "canonical"); len(versions) != 1 {
		t.Fatalf("local data deleted after backend 404: %v", versions)
	}
}
