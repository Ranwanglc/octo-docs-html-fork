package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

// docIDRegistrar is a DocRegistrar that hands out a fixed doc id and counts calls.
type docIDRegistrar struct {
	mu        sync.Mutex
	docID     string
	registers int
}

func (r *docIDRegistrar) Register(context.Context, docsbackend.Registration, string) (*docsbackend.RegistrationResult, error) {
	r.mu.Lock()
	r.registers++
	r.mu.Unlock()
	return &docsbackend.RegistrationResult{DocID: r.docID, OctoDocSlug: "octo-doc", ShareURL: "https://share/" + r.docID, Created: true}, nil
}

func (*docIDRegistrar) Rename(context.Context, string, string, string) {}
func (*docIDRegistrar) Delete(context.Context, string, string)         {}

func (r *docIDRegistrar) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registers
}

// docIDWriteFailStore wraps a MetadataStore and fails ONLY the meta write that
// carries a docs-backend doc id. Every other PutMeta (notably publish's own
// upsertMeta) still succeeds, so the publish commits normally and only the
// doc-id persistence step is broken — exactly the failure mode we must survive.
type docIDWriteFailStore struct {
	storage.MetadataStore
	mu       sync.Mutex
	rejected int
}

func (s *docIDWriteFailStore) PutMeta(ctx context.Context, slug string, meta storage.DocMeta) error {
	if docID, _ := meta.Extra[storage.DocsBackendDocIDExtraKey].(string); docID != "" {
		s.mu.Lock()
		s.rejected++
		s.mu.Unlock()
		return errors.New("meta store unavailable")
	}
	return s.MetadataStore.PutMeta(ctx, slug, meta)
}

func (s *docIDWriteFailStore) rejects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejected
}

// docIDWriteCountStore counts meta writes that FIRST introduce a docs-backend
// doc id, so a test can prove persistence is idempotent.
//
// Counting every doc-id-bearing PutMeta would not work: once the id is stored,
// each later republish rewrites the whole meta (doc id included) through the
// normal publish path, which is expected and unrelated to persistence. Only a
// write that changes the stored id from absent/different to the new value counts.
type docIDWriteCountStore struct {
	storage.MetadataStore
	mu     sync.Mutex
	writes int
}

func (s *docIDWriteCountStore) PutMeta(ctx context.Context, slug string, meta storage.DocMeta) error {
	if docID, _ := meta.Extra[storage.DocsBackendDocIDExtraKey].(string); docID != "" {
		prev := ""
		if before, err := s.GetMeta(ctx, slug); err == nil && before != nil {
			prev, _ = before.Extra[storage.DocsBackendDocIDExtraKey].(string)
		}
		if prev != docID {
			s.mu.Lock()
			s.writes++
			s.mu.Unlock()
		}
	}
	return s.MetadataStore.PutMeta(ctx, slug, meta)
}

func (s *docIDWriteCountStore) docIDWrites() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func storedDocID(t *testing.T, store storage.MetadataStore, slug string) string {
	t.Helper()
	meta, err := store.GetMeta(context.Background(), slug)
	if err != nil {
		t.Fatalf("GetMeta(%q): %v", slug, err)
	}
	if meta == nil {
		t.Fatalf("GetMeta(%q) = nil, want meta", slug)
	}
	_, docID, _ := meta.DocsBackendRegistration()
	return docID
}

func newDocSvc(store storage.MetadataStore, blobs storage.BlobStore, reg service.DocRegistrar) *service.DocService {
	locker := sluglock.NewMemory()
	return service.NewDocService(blobs, store, service.NewCommentService(store, locker), locker, "", 5<<20).
		WithDocsBackendRegistration(reg, nil)
}

// Scenario 1: a bot publish (no userPublish provenance) must persist the
// backend-assigned doc id into meta, not merely echo it in the response. Before
// this change the persistence was gated behind `if result.userPublish`, so the
// bot path lost the doc id on process restart.
func TestBotPublishPersistsDocsBackendDocID(t *testing.T) {
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	registrar := &docIDRegistrar{docID: "doc-bot-1"}
	ds := newDocSvc(store, store, registrar)

	res, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "bot-doc", HTML: "<html><body>hi</body></html>", Title: "Bot", MountType: "group", GroupNo: "g1",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Status != "published" || !res.Registered {
		t.Fatalf("result = %+v, want published+registered", res)
	}
	if res.DocID != "doc-bot-1" {
		t.Fatalf("result.DocID = %q, want %q", res.DocID, "doc-bot-1")
	}
	if got := storedDocID(t, store, "bot-doc"); got != "doc-bot-1" {
		t.Fatalf("persisted doc id = %q, want %q", got, "doc-bot-1")
	}
}

// Scenario 2: republishing the same slug is idempotent — the stored doc id stays
// the same and no extra doc-id write happens (the value is already present, so
// persistDocsBackendDocID short-circuits before PutMeta).
func TestBotPublishDocIDPersistIsIdempotent(t *testing.T) {
	base := memory.New()
	t.Cleanup(func() { _ = base.Close() })
	store := &docIDWriteCountStore{MetadataStore: base}
	registrar := &docIDRegistrar{docID: "doc-bot-2"}
	ds := newDocSvc(store, base, registrar)

	for i := range 3 {
		res, err := ds.Publish(context.Background(), service.PublishInput{
			Slug: "idem-doc", HTML: "<html><body>hi</body></html>", Title: "Idem", MountType: "group", GroupNo: "g1",
		})
		if err != nil {
			t.Fatalf("Publish #%d: %v", i+1, err)
		}
		if res.DocID != "doc-bot-2" {
			t.Fatalf("Publish #%d DocID = %q, want %q", i+1, res.DocID, "doc-bot-2")
		}
		if got := storedDocID(t, base, "idem-doc"); got != "doc-bot-2" {
			t.Fatalf("Publish #%d persisted doc id = %q, want %q", i+1, got, "doc-bot-2")
		}
	}
	if registrar.calls() != 3 {
		t.Fatalf("registrar calls = %d, want 3", registrar.calls())
	}
	// Exactly one doc-id-INTRODUCING write across three publishes: the first
	// records it, the later ones short-circuit on the identical value. (Later
	// publishes still rewrite meta wholesale, but they carry the same id, so they
	// are not counted — see docIDWriteCountStore.)
	if got := store.docIDWrites(); got != 1 {
		t.Fatalf("doc-id introducing meta writes = %d, want 1 (persistence must be idempotent)", got)
	}
}

// Scenario 3: a document that already carries a *different* doc id keeps it. The
// backend identity is never silently rewritten (see #44); the mismatch is warned
// about and the stored value survives.
func TestBotPublishDoesNotOverwriteDifferentDocID(t *testing.T) {
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	// Seed an existing document that already has a doc id from an earlier life.
	if err := store.PutMeta(ctx, "owned-doc", storage.DocMeta{
		Slug:  "owned-doc",
		Title: "Owned",
		Extra: map[string]any{storage.DocsBackendDocIDExtraKey: "doc-original"},
	}); err != nil {
		t.Fatalf("PutMeta: %v", err)
	}

	registrar := &docIDRegistrar{docID: "doc-intruder"}
	ds := newDocSvc(store, store, registrar)

	res, err := ds.Publish(ctx, service.PublishInput{
		Slug: "owned-doc", HTML: "<html><body>hi</body></html>", Title: "Owned", MountType: "group", GroupNo: "g1",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Status != "published" {
		t.Fatalf("status = %q, want published", res.Status)
	}
	if got := storedDocID(t, store, "owned-doc"); got != "doc-original" {
		t.Fatalf("persisted doc id = %q, want the original %q to be preserved", got, "doc-original")
	}
}

// Scenario 4: when doc-id persistence fails the publish MUST still report
// "published". The document bytes and the backend registration are already
// committed; only the local doc-id convenience record is missing and the next
// write self-heals it. Downgrading to registration_failed here would make the
// caller retry a successful publish.
func TestBotPublishSurvivesDocIDPersistFailure(t *testing.T) {
	base := memory.New()
	t.Cleanup(func() { _ = base.Close() })
	store := &docIDWriteFailStore{MetadataStore: base}
	registrar := &docIDRegistrar{docID: "doc-bot-4"}
	ds := newDocSvc(store, base, registrar)

	ctx := context.Background()
	res, err := ds.Publish(ctx, service.PublishInput{
		Slug: "fragile-doc", HTML: "<html><body>v1</body></html>", Title: "Fragile", MountType: "group", GroupNo: "g1",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// The publish itself is a success and must not be downgraded.
	if res.Status != "published" {
		t.Fatalf("status = %q, want published (doc-id persist failure must not fail publish)", res.Status)
	}
	if !res.Registered || res.DocID != "doc-bot-4" {
		t.Fatalf("result = %+v, want registered with doc id doc-bot-4", res)
	}
	// The doc-id write really was attempted and really did fail...
	if store.rejects() == 0 {
		t.Fatal("no doc-id meta write was attempted; the failure path was not exercised")
	}
	// ...so nothing was persisted locally; the next write self-heals it.
	if got := storedDocID(t, base, "fragile-doc"); got != "" {
		t.Fatalf("persisted doc id = %q, want empty after the write failed", got)
	}
}
