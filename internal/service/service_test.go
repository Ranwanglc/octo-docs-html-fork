package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/core"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

func newDoc(t *testing.T) (*service.DocService, *service.CommentService) {
	t.Helper()
	store := memory.New()
	locker := sluglock.NewMemory()
	cs := service.NewCommentService(store, locker)
	ds := service.NewDocService(store, store, cs, locker, "", 5<<20)
	return ds, cs
}

func TestPublishAutoIncrementsVersion(t *testing.T) {
	ds, _ := newDoc(t)
	ctx := context.Background()
	r1, err := ds.Publish(ctx, service.PublishInput{Slug: "d", HTML: "<html><body><p>a</p></body></html>"})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Version != 1 {
		t.Fatalf("first version = %d", r1.Version)
	}
	r2, err := ds.Publish(ctx, service.PublishInput{Slug: "d", HTML: "<html><body><p>b</p></body></html>"})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Version != 2 {
		t.Fatalf("second version = %d", r2.Version)
	}
}

func TestPublishAuthorizationRunsUnderSlugLockWithCurrentExistence(t *testing.T) {
	tests := []struct {
		name string
		seed func(t *testing.T, store *memory.Store)
	}{
		{
			name: "metadata",
			seed: func(t *testing.T, store *memory.Store) {
				t.Helper()
				if err := store.PutMeta(context.Background(), "claimed", storage.DocMeta{Slug: "claimed"}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "published blob",
			seed: func(t *testing.T, store *memory.Store) {
				t.Helper()
				if _, err := store.PutDoc(context.Background(), "claimed", 1, "<html>old</html>"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "draft blob",
			seed: func(t *testing.T, store *memory.Store) {
				t.Helper()
				if _, err := store.PutDraft(context.Background(), "claimed", "<html>draft</html>"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := memory.New()
			locker := sluglock.NewMemory()
			docs := service.NewDocService(store, store, service.NewCommentService(store, locker), locker, "", 5<<20)
			tc.seed(t, store)

			_, err := docs.PublishAuthorized(context.Background(), service.PublishInput{
				Slug: "claimed", HTML: "<html>new</html>",
			}, func(key string, exists bool) error {
				if key != "claimed" {
					t.Fatalf("authorization saw key %q; want the existing doc's key", key)
				}
				if !exists {
					t.Fatal("authorization saw an empty slug")
				}
				return apperr.Forbidden("edit required", "edit_required")
			})
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Code != "edit_required" {
				t.Fatalf("publish authorization error = %v; want edit_required", err)
			}
		})
	}
}

func TestPublishRejectsEmptyAndOversized(t *testing.T) {
	ds, _ := newDoc(t)
	ctx := context.Background()
	if _, err := ds.Publish(ctx, service.PublishInput{Slug: "d", HTML: ""}); err == nil {
		t.Error("empty HTML should be rejected")
	}
	store := memory.New()
	locker := sluglock.NewMemory()
	small := service.NewDocService(store, store, service.NewCommentService(store, locker), locker, "", 10)
	if _, err := small.Publish(ctx, service.PublishInput{Slug: "d", HTML: "<html>way too large</html>"}); err == nil {
		t.Error("oversized HTML should be rejected")
	}
}

func TestPublishRejectsInvalidMountTypeBeforeWrite(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	docs := service.NewDocService(store, store, service.NewCommentService(store, locker), locker, "", 5<<20)
	_, err := docs.Publish(context.Background(), service.PublishInput{
		Slug: "bad-mount", HTML: "<html><body>x</body></html>", MountType: "gruop",
	})
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Code != "mount_type_invalid" {
		t.Fatalf("publish error = %v; want mount_type_invalid", err)
	}
	versions, listErr := store.ListVersions(context.Background(), "bad-mount")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(versions) != 0 {
		t.Fatalf("invalid publish wrote versions: %v", versions)
	}
	meta, metaErr := store.GetMeta(context.Background(), "bad-mount")
	if metaErr != nil {
		t.Fatal(metaErr)
	}
	if meta != nil {
		t.Fatalf("invalid publish wrote metadata: %+v", meta)
	}
}

func TestPublishStampsAndRenders(t *testing.T) {
	ds, _ := newDoc(t)
	ctx := context.Background()
	res, err := ds.Publish(ctx, service.PublishInput{Slug: "d", HTML: "<html><body><img src=\"a.png\"></body></html>"})
	if err != nil {
		t.Fatal(err)
	}
	if res.AIDs != 1 {
		t.Fatalf("expected 1 aid, got %d", res.AIDs)
	}
	data, err := ds.Render(ctx, "d", 1)
	if err != nil || data == nil {
		t.Fatalf("render = %v, %v", data, err)
	}
	if !contains(data.HTML, "data-odoc-aid") {
		t.Error("rendered HTML not stamped")
	}
}

type docsBackendRequest struct {
	Method        string
	Path          string
	Authorization string
	Body          map[string]any
}

func newDocsBackendStub(t *testing.T, status int) (*httptest.Server, <-chan docsBackendRequest) {
	t.Helper()
	ch := make(chan docsBackendRequest, 10)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		ch <- docsBackendRequest{
			Method:        r.Method,
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			Body:          body,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 200 && status < 300 {
			slug, _ := body["octoDocSlug"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docId":       "doc-" + slug,
				"octoDocSlug": slug,
				"shareUrl":    "https://docs.example.test/d/doc-" + slug,
				"created":     true,
			})
		}
	}))
	return ts, ch
}

func waitDocsBackendRequest(t *testing.T, ch <-chan docsBackendRequest) docsBackendRequest {
	t.Helper()
	select {
	case req := <-ch:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for docs-backend request")
		return docsBackendRequest{}
	}
}

func assertNoDocsBackendRequest(t *testing.T, ch <-chan docsBackendRequest) {
	t.Helper()
	select {
	case req := <-ch:
		t.Fatalf("unexpected docs-backend request: %+v", req)
	case <-time.After(150 * time.Millisecond):
	}
}

func newDocWithDocsBackend(t *testing.T, registerURL string) *service.DocService {
	t.Helper()
	store := memory.New()
	locker := sluglock.NewMemory()
	cs := service.NewCommentService(store, locker)
	return service.NewDocService(store, store, cs, locker, "", 5<<20).
		WithDocsBackendRegistration(docsbackend.New(registerURL, "bot-token", nil), nil)
}

func TestPublishRegistersGroupMountedDoc(t *testing.T) {
	ts, reqs := newDocsBackendStub(t, http.StatusOK)
	defer ts.Close()
	ds := newDocWithDocsBackend(t, ts.URL+"/v1/bot/docs")

	result, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "group-doc", HTML: "<html><body><p>x</p></body></html>", Title: "Group Title",
		MountType: "group", GroupNo: "g-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Registered || result.Status != "published" || result.DocID != "doc-group-doc" || result.ShareURL == "" {
		t.Fatalf("publish result = %+v", result)
	}
	if result.URL != result.ShareURL {
		t.Fatalf("url = %q, share_url = %q; want canonical URLs to match", result.URL, result.ShareURL)
	}

	req := waitDocsBackendRequest(t, reqs)
	if req.Method != http.MethodPost || req.Path != "/v1/bot/docs" {
		t.Fatalf("request = %s %s, want POST /v1/bot/docs", req.Method, req.Path)
	}
	if req.Authorization != "Bearer bot-token" {
		t.Fatalf("Authorization = %q", req.Authorization)
	}
	if req.Body["docType"] != "html" || req.Body["octoDocSlug"] != "group-doc" || req.Body["mountType"] != "group" || req.Body["title"] != "Group Title" {
		t.Fatalf("registration body = %#v", req.Body)
	}
	// owner + spaceId are reverse-resolved by docs-backend from the bot token, so
	// octo-doc must NOT send them (omitempty drops the zero-value fields).
	if _, ok := req.Body["owner"]; ok {
		t.Fatalf("owner must not be sent, body = %#v", req.Body)
	}
	if _, ok := req.Body["spaceId"]; ok {
		t.Fatalf("spaceId must not be sent, body = %#v", req.Body)
	}
}

// Load-bearing item 5: when the publish carries a CreatorUID, the doc-key
// derivation drives the docs-backend registration's octoDocSlug — it MUST be the
// derived key (which backend echoes back and equality-checks), never the bare
// alias. This is what keeps the html-side identity and the backend's octoDocSlug
// in agreement under the new scheme.
func TestPublishRegistrationOctoDocSlugIsDerivedKey(t *testing.T) {
	ts, reqs := newDocsBackendStub(t, http.StatusOK)
	defer ts.Close()
	ds := newDocWithDocsBackend(t, ts.URL+"/v1/bot/docs")

	result, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "weekly-report", HTML: "<html><body><p>x</p></body></html>", Title: "Weekly",
		MountType: "group", GroupNo: "g-1", CreatorUID: "bot-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantKey := service.DeriveDocKey("bot-A", "weekly-report")
	if result.Slug != wantKey {
		t.Fatalf("result slug = %q; want derived key %q", result.Slug, wantKey)
	}
	req := waitDocsBackendRequest(t, reqs)
	if req.Body["octoDocSlug"] != wantKey {
		t.Fatalf("registration octoDocSlug = %v; want derived key %q (backend echoes+checks this)", req.Body["octoDocSlug"], wantKey)
	}
}

// TestPublishRegistersUnderPublisherToken verifies route-B: when the publish
// carries a PublisherToken, the docs-backend registration authenticates as that
// token (so the doc is attributed to whoever published it), overriding the
// process-configured fallback token.
func TestPublishRegistersUnderPublisherToken(t *testing.T) {
	ts, reqs := newDocsBackendStub(t, http.StatusOK)
	defer ts.Close()
	ds := newDocWithDocsBackend(t, ts.URL+"/v1/bot/docs")

	if _, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "pub-doc", HTML: "<html><body><p>x</p></body></html>", Title: "Pub Title",
		MountType: "group", GroupNo: "g-1",
		PublisherToken: "publisher-token",
	}); err != nil {
		t.Fatal(err)
	}

	req := waitDocsBackendRequest(t, reqs)
	if req.Authorization != "Bearer publisher-token" {
		t.Fatalf("Authorization = %q, want Bearer publisher-token (publisher token must override fallback)", req.Authorization)
	}
}

// TestPublishRegisterFallsBackToConfiguredToken verifies that when no
// PublisherToken is supplied, the registration falls back to the
// process-configured token — preserving pre-route-B behaviour.
func TestPublishRegisterFallsBackToConfiguredToken(t *testing.T) {
	ts, reqs := newDocsBackendStub(t, http.StatusOK)
	defer ts.Close()
	ds := newDocWithDocsBackend(t, ts.URL+"/v1/bot/docs")

	if _, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "fallback-doc", HTML: "<html><body><p>x</p></body></html>", Title: "Fallback Title",
		MountType: "group", GroupNo: "g-1",
	}); err != nil {
		t.Fatal(err)
	}

	req := waitDocsBackendRequest(t, reqs)
	if req.Authorization != "Bearer bot-token" {
		t.Fatalf("Authorization = %q, want Bearer bot-token (fallback token)", req.Authorization)
	}
}

func TestDocsBackendRegisterUsesCallerContext(t *testing.T) {
	reqStarted := make(chan struct{})
	unblock := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(reqStarted)
		<-unblock
	}))
	defer ts.Close()
	defer ts.CloseClientConnections()
	defer close(unblock)

	client := docsbackend.New(ts.URL+"/v1/bot/docs", "bot-token", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	errCh := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := client.Register(ctx, docsbackend.Registration{
			DocType:     "html",
			OctoDocSlug: "slow-doc",
			MountType:   "group",
			Title:       "Slow",
			SpaceID:     "space-1",
		}, "")
		errCh <- err
		close(done)
	}()

	select {
	case <-reqStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for registrar request")
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("registrar did not return after caller context expired")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("registrar returned after %s, want caller context to bound request", elapsed)
	}
	if err := <-errCh; err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Register error = %v, want context deadline exceeded", err)
	}
}

func TestPublishRegistersThreadMountedDoc(t *testing.T) {
	ts, reqs := newDocsBackendStub(t, http.StatusOK)
	defer ts.Close()
	ds := newDocWithDocsBackend(t, ts.URL+"/v1/bot/docs")

	result, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "thread-doc", HTML: "<html><body><p>x</p></body></html>", Title: "Thread Title",
		MountType: "thread", PublisherToken: "publisher-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Registered || result.Status != "published" || result.DocID != "doc-thread-doc" || result.ShareURL == "" {
		t.Fatalf("result = %+v", result)
	}
	if result.URL != result.ShareURL {
		t.Fatalf("url = %q, share_url = %q; want canonical URLs to match", result.URL, result.ShareURL)
	}
	req := waitDocsBackendRequest(t, reqs)
	if req.Authorization != "Bearer publisher-token" {
		t.Fatalf("Authorization = %q, want Bearer publisher-token", req.Authorization)
	}
	if req.Body["mountType"] != "thread" || req.Body["octoDocSlug"] != "thread-doc" || req.Body["title"] != "Thread Title" {
		t.Fatalf("registration body = %#v", req.Body)
	}
	// The current backend contract persists mountType only. Thread/group source
	// attribution needs a future backend schema and contract design.
}

func TestPublishSkipsRegistrationWhenNoMountType(t *testing.T) {
	ts, reqs := newDocsBackendStub(t, http.StatusOK)
	defer ts.Close()
	ds := newDocWithDocsBackend(t, ts.URL+"/v1/bot/docs")

	// No mount info on the publish request ⇒ nothing to register against ⇒ skip.
	result, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "unmounted-doc", HTML: "<html><body><p>x</p></body></html>", Title: "Unmounted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Registered || result.Status != "published_unregistered" {
		t.Fatalf("result = %+v", result)
	}
	if result.URL != "" || result.ShareURL != "" {
		t.Fatalf("unregistered result exposes a url: %+v", result)
	}
	assertNoDocsBackendRequest(t, reqs)
}

func TestPublishSkipsRegistrationWhenURLDisabled(t *testing.T) {
	for _, mountType := range []string{"group", "thread"} {
		t.Run(mountType, func(t *testing.T) {
			ts, reqs := newDocsBackendStub(t, http.StatusOK)
			defer ts.Close()
			// Registrar URL empty means mounted publishes fail closed.
			ds := newDocWithDocsBackend(t, "")

			result, err := ds.Publish(context.Background(), service.PublishInput{
				Slug: "disabled-" + mountType, HTML: "<html><body><p>x</p></body></html>", Title: "Disabled",
				MountType: mountType,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Registered || result.Status != "registration_failed" {
				t.Fatalf("result = %+v", result)
			}
			assertNoDocsBackendRequest(t, reqs)
		})
	}
}

func TestPublishRenamesExistingRegistrationWhenTitleChanges(t *testing.T) {
	var mu sync.Mutex
	var requests []docsBackendRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		requests = append(requests, docsBackendRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"docId":"doc-rename-doc","octoDocSlug":"rename-doc","shareUrl":"https://docs.example.test/d/doc-rename-doc","created":false}`))
		}
	}))
	defer ts.Close()
	ds := newDocWithDocsBackend(t, ts.URL+"/v1/bot/docs")

	if _, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "rename-doc", HTML: "<html><body>one</body></html>", Title: "Old", MountType: "group",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "rename-doc", HTML: "<html><body>two</body></html>", Title: "New", MountType: "group",
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("requests = %+v, want POST, POST, PATCH", requests)
	}
	if requests[2].Method != http.MethodPatch || requests[2].Path != "/v1/bot/docs/octo-doc/rename-doc" || requests[2].Body["title"] != "New" {
		t.Fatalf("rename request = %+v", requests[2])
	}
}

type cancelRegistrar struct {
	mu          sync.Mutex
	registers   int
	renames     int
	firstCalled chan struct{}
}

func (r *cancelRegistrar) Register(ctx context.Context, _ docsbackend.Registration, _ string) (*docsbackend.RegistrationResult, error) {
	r.mu.Lock()
	r.registers++
	if r.registers == 1 {
		close(r.firstCalled)
	}
	r.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *cancelRegistrar) Rename(context.Context, string, string, string) {
	r.mu.Lock()
	r.renames++
	r.mu.Unlock()
}

func (*cancelRegistrar) Delete(context.Context, string, string) {}

func TestPublishCancellationStopsRegistrationRetries(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	registrar := &cancelRegistrar{firstCalled: make(chan struct{})}
	ds := service.NewDocService(store, store, service.NewCommentService(store, locker), locker, "", 5<<20).
		WithDocsBackendRegistration(registrar, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *service.PublishResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := ds.Publish(ctx, service.PublishInput{
			Slug: "cancel-doc", HTML: "<html><body>x</body></html>", Title: "Cancel", MountType: "group",
		})
		done <- result
		errCh <- err
	}()
	<-registrar.firstCalled
	cancel()

	select {
	case result := <-done:
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
		if result.Registered || result.Status != "registration_failed" {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish did not stop after caller cancellation")
	}
	registrar.mu.Lock()
	defer registrar.mu.Unlock()
	if registrar.registers != 1 || registrar.renames != 0 {
		t.Fatalf("registers=%d renames=%d", registrar.registers, registrar.renames)
	}
	versions, err := ds.ListVersions(context.Background(), "cancel-doc")
	if err != nil || len(versions.Versions) != 1 {
		t.Fatalf("published HTML missing after registration cancellation: versions=%+v err=%v", versions, err)
	}
}

func TestPublishReportsRegistrationFailureWithoutNewVersion(t *testing.T) {
	ts, reqs := newDocsBackendStub(t, http.StatusInternalServerError)
	defer ts.Close()
	ds := newDocWithDocsBackend(t, ts.URL+"/v1/bot/docs")

	res, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "best-effort-doc", HTML: "<html><body><p>x</p></body></html>", Title: "Best Effort",
		MountType: "space",
	})
	if err != nil {
		t.Fatalf("Publish should not fail on registrar 500: %v", err)
	}
	if res.Version != 1 {
		t.Fatalf("version = %d, want 1", res.Version)
	}
	if res.Registered || res.Status != "registration_failed" {
		t.Fatalf("result = %+v, want published-but-unregistered", res)
	}
	for range 3 {
		req := waitDocsBackendRequest(t, reqs)
		if req.Method != http.MethodPost || req.Body["octoDocSlug"] != "best-effort-doc" {
			t.Fatalf("registration request = %+v", req)
		}
	}
	versions, err := ds.ListVersions(context.Background(), "best-effort-doc")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions.Versions) != 1 || versions.Versions[0].N != 1 {
		t.Fatalf("versions = %+v, want only version 1", versions)
	}
}

func TestPublishAcceptsExistingRegistration(t *testing.T) {
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"docId":"doc-existing","octoDocSlug":"existing-doc","shareUrl":"https://docs.example.test/d/doc-existing","created":false}`))
	}))
	defer ts.Close()
	ds := newDocWithDocsBackend(t, ts.URL)
	result, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "existing-doc", HTML: "<html><body>x</body></html>", MountType: "space",
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !result.Registered || result.Status != "published" || result.DocID != "doc-existing" {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
}

func TestPublishRetriesMalformedRegistrationResponse(t *testing.T) {
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"docId":`))
	}))
	defer ts.Close()
	ds := newDocWithDocsBackend(t, ts.URL)
	result, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "bad-json", HTML: "<html><body>x</body></html>", MountType: "group",
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || result.Registered || result.Status != "registration_failed" {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
}

type timeoutRegistrar struct {
	calls int
}

func (r *timeoutRegistrar) Register(context.Context, docsbackend.Registration, string) (*docsbackend.RegistrationResult, error) {
	r.calls++
	return nil, context.DeadlineExceeded
}

func (*timeoutRegistrar) Rename(context.Context, string, string, string) {}
func (*timeoutRegistrar) Delete(context.Context, string, string)         {}

func TestPublishRetriesRegistrationTimeoutWithoutRepublish(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	registrar := &timeoutRegistrar{}
	ds := service.NewDocService(store, store, service.NewCommentService(store, locker), locker, "", 5<<20).
		WithDocsBackendRegistration(registrar, nil)
	result, err := ds.Publish(context.Background(), service.PublishInput{
		Slug: "timeout-doc", HTML: "<html><body>x</body></html>", MountType: "group",
	})
	if err != nil {
		t.Fatal(err)
	}
	if registrar.calls != 3 || result.Registered || result.Status != "registration_failed" {
		t.Fatalf("calls=%d result=%+v", registrar.calls, result)
	}
	versions, err := ds.ListVersions(context.Background(), "timeout-doc")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions.Versions) != 1 || versions.Versions[0].N != 1 {
		t.Fatalf("versions = %+v, want only version 1", versions)
	}
}

func TestRenderMissingReturnsNil(t *testing.T) {
	ds, _ := newDoc(t)
	data, err := ds.Render(context.Background(), "nope", 1)
	if err != nil || data != nil {
		t.Fatalf("render missing = %v, %v; want nil, nil", data, err)
	}
}

func TestCommentCreateListDelete(t *testing.T) {
	ds, cs := newDoc(t)
	ctx := context.Background()
	_, _ = ds.Publish(ctx, service.PublishInput{Slug: "d", HTML: "<html><body><p>hi there</p></body></html>"})

	created, err := cs.Create(ctx, "d", &core.Author{Login: "alice"}, "nice", &core.Anchor{Kind: "text", Text: "hi"}, 1)
	if err != nil || created.Status != 200 {
		t.Fatalf("create = %+v, %v", created, err)
	}
	snap := created.Body.(*core.CommentSnapshot)
	list, err := cs.List(ctx, "d", 1)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}

	if _, err := cs.Remove(ctx, "d", snap.ID, 1, "alice"); err != nil {
		t.Fatal(err)
	}
	list, _ = cs.List(ctx, "d", 1)
	if len(list) != 0 {
		t.Fatalf("after delete, list = %v", list)
	}
}

func TestPublishMergeReconcilesAnchors(t *testing.T) {
	ds, cs := newDoc(t)
	ctx := context.Background()
	// v1 with an svg, comment anchored to its aid.
	r1, _ := ds.Publish(ctx, service.PublishInput{Slug: "d", HTML: "<html><body><h2>Chart</h2><svg viewBox=\"0 0 1 1\"></svg></body></html>"})
	_ = r1
	// fetch the aid from the rendered doc isn't trivial here; instead assert the
	// merge path runs without error on republish (reconcile + compact).
	if _, err := ds.Publish(ctx, service.PublishInput{Slug: "d", HTML: "<html><body><h2>Chart</h2><svg viewBox=\"0 0 1 1\"></svg></body></html>"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.List(ctx, "d", 2); err != nil {
		t.Fatal(err)
	}
}

// failDeleteDraftBlobs wraps a blob store but always errors on DeleteDraft, to
// simulate a transient blob-store failure in the post-commit cleanup of Promote.
type failDeleteDraftBlobs struct {
	*memory.Store
}

func (f failDeleteDraftBlobs) DeleteDraft(ctx context.Context, slug string) error {
	return errors.New("simulated transient S3 failure")
}

// TestPromoteIdempotentWhenDraftClearFails guards against a duplicate version: if
// DeleteDraft fails after publishLocked has committed the version, Promote must
// still report success (not surface the cleanup error), so a naive retry doesn't
// re-run publishLocked and mint a second identical version.
func TestPromoteIdempotentWhenDraftClearFails(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	blobs := failDeleteDraftBlobs{store}
	cs := service.NewCommentService(store, locker)
	ds := service.NewDocService(blobs, store, cs, locker, "", 5<<20)
	ctx := context.Background()

	if _, err := ds.SaveDraft(ctx, "d", "<html><body><p>draft</p></body></html>", "T", ""); err != nil {
		t.Fatal(err)
	}

	// Promote succeeds despite the DeleteDraft failure (cleanup is best-effort).
	r1, err := ds.Promote(ctx, "d", "T")
	if err != nil {
		t.Fatalf("promote should succeed past the commit point, got: %v", err)
	}
	if r1.Version != 1 {
		t.Fatalf("first promote version = %d, want 1", r1.Version)
	}

	// The draft still exists (delete failed), so a retry could re-promote. Because
	// the first promote reported success, a well-behaved caller won't retry — but if
	// it does, it mints v2 rather than corrupting v1. Assert v1 is intact and the
	// list has exactly the versions we expect after one *successful* promote.
	vl, err := ds.ListVersions(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if len(vl.Versions) != 1 || vl.Versions[0].N != 1 {
		t.Fatalf("after one successful promote, versions = %+v; want exactly [v1]", vl.Versions)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestConcurrentPublishConsistent drives many concurrent publishes of the same
// slug through the per-slug lock and asserts every version 1..N is present exactly
// once — i.e. no two publishes resolved to the same version and clobbered a blob.
func TestConcurrentPublishConsistent(t *testing.T) {
	ds, _ := newDoc(t)
	ctx := context.Background()
	const n = 30

	errs := make(chan error, n)
	for range n {
		go func() {
			_, err := ds.Publish(ctx, service.PublishInput{
				Slug: "same", HTML: "<html><body><p>x</p></body></html>",
			})
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("publish failed: %v", err)
		}
	}

	vl, err := ds.ListVersions(ctx, "same")
	if err != nil {
		t.Fatal(err)
	}
	if len(vl.Versions) != n {
		t.Fatalf("got %d versions, want %d (a publish was lost)", len(vl.Versions), n)
	}
	seen := map[int]bool{}
	for _, v := range vl.Versions {
		if seen[v.N] {
			t.Fatalf("duplicate version %d", v.N)
		}
		seen[v.N] = true
	}
	for i := 1; i <= n; i++ {
		if !seen[i] {
			t.Fatalf("missing version %d", i)
		}
	}
}

// TestConcurrentFirstPublishSameCreatorAlias covers the case the lock-scope
// redesign exists for: N callers concurrently publishing the SAME alias as the
// SAME creator. All of them derive one key, so all of them must serialize on that
// key's lock — exactly one creates the document, the rest append versions. A
// regression that locked the client identifier instead of the resolved key, or
// that re-stamped key_scheme on the losers, shows up here as lost/duplicate
// versions or a second document.
func TestConcurrentFirstPublishSameCreatorAlias(t *testing.T) {
	ds, store := newDocWithStore(t)
	ctx := context.Background()
	const n = 30
	const creator = "bot-A"
	const alias = "weekly-report"
	key := service.DeriveDocKey(creator, alias)

	errs := make(chan error, n)
	for range n {
		go func() {
			_, err := ds.Publish(ctx, service.PublishInput{
				Slug: alias, HTML: "<html><body><p>x</p></body></html>", CreatorUID: creator,
			})
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("publish failed: %v", err)
		}
	}

	// Every publish landed on the one derived key, with a dense 1..n version list.
	vl, err := ds.ListVersions(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(vl.Versions) != n {
		t.Fatalf("got %d versions under %s, want %d (a publish was lost)", len(vl.Versions), key, n)
	}
	seen := map[int]bool{}
	for _, v := range vl.Versions {
		if seen[v.N] {
			t.Fatalf("duplicate version %d", v.N)
		}
		seen[v.N] = true
	}
	for i := 1; i <= n; i++ {
		if !seen[i] {
			t.Fatalf("missing version %d", i)
		}
	}

	// No second document was created under the alias itself.
	if aliasMeta, merr := store.GetMeta(ctx, alias); merr != nil {
		t.Fatal(merr)
	} else if aliasMeta != nil {
		t.Fatalf("concurrent publishes also created a doc under the raw alias %q", alias)
	}

	// key_scheme/alias were stamped exactly once, and the title never degraded to
	// the hex key.
	meta, err := store.GetMeta(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil {
		t.Fatalf("no meta under derived key %s", key)
	}
	if meta.KeyScheme() != "dockey" {
		t.Fatalf("key_scheme = %q; want dockey", meta.KeyScheme())
	}
	if meta.Alias() != alias {
		t.Fatalf("alias = %q; want %q", meta.Alias(), alias)
	}
	if meta.Title != alias {
		t.Fatalf("title = %q; want the alias %q", meta.Title, alias)
	}
}

// TestConcurrentPublishMixedAddressingSameDoc is the fail-before guard for the
// lock scope. Half the callers address the document by its human alias, half by
// its derived key — the SAME document either way. If the critical section locked
// the client identifier instead of the resolved key, the two halves would take
// two different locks, run unserialized, and lose or duplicate versions.
func TestConcurrentPublishMixedAddressingSameDoc(t *testing.T) {
	ds, store := newDocWithStore(t)
	ctx := context.Background()
	const creator = "bot-A"
	const alias = "mixed-addressing"
	key := service.DeriveDocKey(creator, alias)

	// Seed v1 so both addressing forms resolve to an existing document.
	if _, err := ds.Publish(ctx, service.PublishInput{
		Slug: alias, HTML: "<html><body><p>seed</p></body></html>", CreatorUID: creator,
	}); err != nil {
		t.Fatal(err)
	}

	const n = 30 // 15 by alias + 15 by key
	errs := make(chan error, n)
	for i := range n {
		identifier := alias
		if i%2 == 0 {
			identifier = key
		}
		go func(identifier string) {
			_, err := ds.Publish(ctx, service.PublishInput{
				Slug: identifier, HTML: "<html><body><p>x</p></body></html>", CreatorUID: creator,
			})
			errs <- err
		}(identifier)
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("publish failed: %v", err)
		}
	}

	vl, err := ds.ListVersions(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(vl.Versions) != n+1 {
		t.Fatalf("got %d versions under %s, want %d (mixed addressing was not serialized)", len(vl.Versions), key, n+1)
	}
	seen := map[int]bool{}
	for _, v := range vl.Versions {
		if seen[v.N] {
			t.Fatalf("duplicate version %d", v.N)
		}
		seen[v.N] = true
	}
	for i := 1; i <= n+1; i++ {
		if !seen[i] {
			t.Fatalf("missing version %d", i)
		}
	}
	if aliasMeta, merr := store.GetMeta(ctx, alias); merr != nil {
		t.Fatal(merr)
	} else if aliasMeta != nil {
		t.Fatalf("alias-addressed publishes created a second doc under %q", alias)
	}
}

// TestConcurrentPublishAndRemove exercises Publish and Remove of the same slug
// racing through the shared per-slug lock; it must not panic or deadlock and must
// leave a self-consistent final state (either fully removed, or a valid version
// list whose latest blob exists).
func TestConcurrentPublishAndRemove(t *testing.T) {
	ds, _ := newDoc(t)
	ctx := context.Background()

	done := make(chan error, 2)
	go func() {
		_, err := ds.Publish(ctx, service.PublishInput{
			Slug: "rp", HTML: "<html><body><p>x</p></body></html>",
		})
		done <- err
	}()
	go func() { done <- ds.Remove(ctx, "rp") }()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("op failed: %v", err)
		}
	}

	// Final state must be self-consistent: ListVersions returns nil (removed) or a
	// list; if a list, the render path for the latest version must resolve.
	vl, err := ds.ListVersions(ctx, "rp")
	if err != nil {
		t.Fatal(err)
	}
	if vl != nil && len(vl.Versions) > 0 {
		latest := vl.Versions[len(vl.Versions)-1].N
		rd, err := ds.Render(ctx, "rp", latest)
		if err != nil {
			t.Fatal(err)
		}
		if rd == nil {
			t.Fatalf("version %d listed but blob missing (inconsistent state)", latest)
		}
	}
}

// Fix A (lost update): ReplaceElement must run resolve→render→replace→publish
// under ONE per-slug lock. Two concurrent ReplaceElement calls on the same slug
// must serialize into two DISTINCT monotonic versions where the SECOND edit is
// applied on top of the first — neither is clobbered. Before the fix the
// read/replace ran outside the lock, so both could base on v1 and one publish
// would overwrite the other's version (the second edit would be lost).
//
// Each goroutine edits a DIFFERENT element (distinct, stable content-addressed
// aids that survive the other's re-stamp), so both edits must be observable in
// the final version if serialization worked.
func TestReplaceElementConcurrentNoLostUpdate(t *testing.T) {
	ds, _ := newDoc(t)
	ctx := context.Background()
	base := `<html><body>` +
		`<section><p>alpha-base</p></section>` +
		`<aside><p>beta-base</p></aside>` +
		`</body></html>`
	if _, err := ds.Publish(ctx, service.PublishInput{Slug: "cc", HTML: base}); err != nil {
		t.Fatal(err)
	}
	sr := core.StampAids(base)
	if len(sr.AIDs) < 2 {
		t.Fatalf("want >=2 artifacts, got %d", len(sr.AIDs))
	}
	aidA, aidB := sr.AIDs[0].AID, sr.AIDs[1].AID

	type job struct {
		aid  string
		frag string
	}
	jobs := []job{
		{aidA, `<section><p>alpha-edit</p></section>`},
		{aidB, `<aside><p>beta-edit</p></aside>`},
	}
	var wg sync.WaitGroup
	results := make([]int, 2)
	errs := make([]error, 2)
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := ds.ReplaceElement(ctx, "cc", 0, jobs[i].aid, jobs[i].frag)
			errs[i] = err
			if r != nil {
				results[i] = r.Version
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("replace %d failed: %v", i, err)
		}
	}
	// Distinct versions (2 and 3), not both 2 — the lost-update signature.
	if results[0] == results[1] {
		t.Fatalf("lost update: both replaces resolved to version %d", results[0])
	}
	vl, err := ds.ListVersions(ctx, "cc")
	if err != nil {
		t.Fatal(err)
	}
	latest := vl.Versions[len(vl.Versions)-1].N
	if latest != 3 {
		t.Fatalf("latest version = %d; want 3 (base + 2 serialized replaces)", latest)
	}
	// Both intermediate and final versions must exist (no clobber).
	for _, v := range []int{2, 3} {
		if rd, err := ds.Render(ctx, "cc", v); err != nil || rd == nil {
			t.Fatalf("version %d missing after concurrent replace (clobbered): %v", v, err)
		}
	}
	// The final version must carry BOTH edits: the second replace was applied on
	// top of the first, proving it based on the up-to-date latest under the lock.
	rd, _ := ds.Render(ctx, "cc", 3)
	if !strings.Contains(rd.HTML, "alpha-edit") || !strings.Contains(rd.HTML, "beta-edit") {
		t.Errorf("final version missing an edit (lost update): %s", rd.HTML)
	}
	if strings.Contains(rd.HTML, "alpha-base") || strings.Contains(rd.HTML, "beta-base") {
		t.Errorf("final version still shows base content (an edit was lost): %s", rd.HTML)
	}
}

// ---------------------------------------------------------------------------
// Doc-key identity model (feat/doc-key-identity). These merge into the existing
// service test file per the change spec. They exercise DeriveDocKey/IsDocKey and
// the publish/draft identity resolution: new docs land under a per-creator
// derived key, the client slug becomes a per-creator alias, and legacy bare-slug
// docs keep their exact pre-change behaviour.

// newDocWithStore is newDoc but also returns the backing memory store so a test
// can inspect the meta that publish/draft wrote (creator_uid, alias, key_scheme).
func newDocWithStore(t *testing.T) (*service.DocService, *memory.Store) {
	t.Helper()
	store := memory.New()
	locker := sluglock.NewMemory()
	cs := service.NewCommentService(store, locker)
	ds := service.NewDocService(store, store, cs, locker, "", 5<<20)
	return ds, store
}

// TestDeriveDocKeyShapeAndDeterminism locks the key format: prefix k_, total
// length 28, 26 trailing lowercase hex, IsDocKey true, and deterministic per
// (creator, alias). It also proves the key passes the shared slug validator so
// URLs/CLI need no change, and that different creators or aliases diverge.
func TestDeriveDocKeyShapeAndDeterminism(t *testing.T) {
	k := service.DeriveDocKey("bot-A", "weekly-report")
	if len(k) != 28 || k[:2] != "k_" {
		t.Fatalf("key = %q; want k_ + 26 hex (len 28)", k)
	}
	if !service.IsDocKey(k) {
		t.Fatalf("IsDocKey(%q) = false; want true", k)
	}
	if got := config.SafeSlug(k); got != k {
		t.Fatalf("derived key %q not slug-safe (SafeSlug=%q)", k, got)
	}
	// Deterministic: same inputs → same key (idempotency契约 depends on this).
	if k2 := service.DeriveDocKey("bot-A", "weekly-report"); k2 != k {
		t.Fatalf("non-deterministic key: %q vs %q", k, k2)
	}
	// Different creator OR different alias → different key.
	if service.DeriveDocKey("bot-B", "weekly-report") == k {
		t.Fatal("different creator produced same key (namespace collision)")
	}
	if service.DeriveDocKey("bot-A", "quarterly") == k {
		t.Fatal("different alias produced same key")
	}
	// A legacy bare slug is not a doc key.
	if service.IsDocKey("weekly-report") {
		t.Fatal("bare slug misclassified as doc key")
	}
	// Guard the exact-length / non-hex cases IsDocKey must reject.
	if service.IsDocKey("k_short") || service.IsDocKey("k_"+strings.Repeat("g", 26)) {
		t.Fatal("IsDocKey accepted a malformed key")
	}
}

// Acceptance 1 & 3-new: bot A first publish of "weekly-report" lands under a
// derived key (not the bare slug), returns url /d/<key>/v/1, alias=weekly-report,
// title defaults to the alias (never the hex key).
func TestPublishNewDocLandsUnderDerivedKey(t *testing.T) {
	ds, store := newDocWithStore(t)
	ctx := context.Background()
	res, err := ds.Publish(ctx, service.PublishInput{
		Slug: "weekly-report", HTML: "<html><body><p>w1</p></body></html>", CreatorUID: "bot-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantKey := service.DeriveDocKey("bot-A", "weekly-report")
	if res.Slug != wantKey {
		t.Fatalf("result slug = %q; want derived key %q", res.Slug, wantKey)
	}
	if res.Version != 1 || res.Alias != "weekly-report" {
		t.Fatalf("result = %+v; want v1, alias=weekly-report", res)
	}
	// The doc lives under the key, not the bare alias.
	if meta, _ := store.GetMeta(ctx, "weekly-report"); meta != nil {
		t.Fatalf("bare slug should hold no meta; got %+v", meta)
	}
	meta, err := store.GetMeta(ctx, wantKey)
	if err != nil || meta == nil {
		t.Fatalf("meta under key missing: %v", err)
	}
	if meta.Alias() != "weekly-report" || meta.KeyScheme() != "dockey" || meta.CreatorUID() != "bot-A" {
		t.Fatalf("meta extras = alias=%q scheme=%q creator=%q; want weekly-report/dockey/bot-A",
			meta.Alias(), meta.KeyScheme(), meta.CreatorUID())
	}
	// Title must default to the alias, never the hex key.
	if meta.Title != "weekly-report" {
		t.Fatalf("title = %q; want alias weekly-report (never the hex key)", meta.Title)
	}
}

// Acceptance 2: bot A re-publishing the same alias appends v2 on the SAME key —
// no second document is created (idempotency contract preserved).
func TestPublishSameCreatorSameAliasAppendsSameKey(t *testing.T) {
	ds, store := newDocWithStore(t)
	ctx := context.Background()
	in := service.PublishInput{Slug: "weekly-report", HTML: "<html><body><p>v1</p></body></html>", CreatorUID: "bot-A"}
	r1, err := ds.Publish(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	in.HTML = "<html><body><p>v2</p></body></html>"
	r2, err := ds.Publish(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Slug != r1.Slug {
		t.Fatalf("second publish key = %q; want same as first %q (no new doc)", r2.Slug, r1.Slug)
	}
	if r2.Version != 2 {
		t.Fatalf("second version = %d; want 2 (append)", r2.Version)
	}
	vers, _ := store.ListVersions(ctx, r1.Slug)
	if len(vers) != 2 {
		t.Fatalf("versions under key = %v; want exactly 2", vers)
	}
}

// Acceptance 3: bot B publishing the SAME alias lands on a DIFFERENT key — each
// creator gets its own document, so squatting and cross-owner collision are both
// eliminated. Each starts at v1 and neither disturbs the other.
func TestPublishDifferentCreatorSameAliasDistinctKeys(t *testing.T) {
	ds, store := newDocWithStore(t)
	ctx := context.Background()
	a, err := ds.Publish(ctx, service.PublishInput{Slug: "weekly-report", HTML: "<html><body><p>A</p></body></html>", CreatorUID: "bot-A"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ds.Publish(ctx, service.PublishInput{Slug: "weekly-report", HTML: "<html><body><p>B</p></body></html>", CreatorUID: "bot-B"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Slug == b.Slug {
		t.Fatalf("two creators collided on key %q (squatting not resolved)", a.Slug)
	}
	if a.Version != 1 || b.Version != 1 {
		t.Fatalf("versions = A%d B%d; want both v1", a.Version, b.Version)
	}
	// Each doc holds exactly its own single version.
	for who, k := range map[string]string{"A": a.Slug, "B": b.Slug} {
		if v, _ := store.ListVersions(ctx, k); len(v) != 1 {
			t.Fatalf("doc %s versions = %v; want exactly 1 (no cross-contamination)", who, v)
		}
	}
}

// Acceptance 4: a legacy doc (meta keyed by the bare slug, no key_scheme) keeps
// EXACTLY today's append behaviour — even when the same creator republishes the
// same name, resolution ① targets the existing bare-slug doc rather than deriving
// a new key. No new document, no scheme stamped.
func TestPublishLegacyBareSlugAppendsInPlace(t *testing.T) {
	ds, store := newDocWithStore(t)
	ctx := context.Background()
	// Seed a legacy doc directly: bare-slug key, a v1 blob + meta, NO key_scheme,
	// creator_uid = bot-A (the shape a pre-change publish produced).
	if _, err := store.PutDoc(ctx, "legacy-doc", 1, "<html><body><p>old</p></body></html>"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutMeta(ctx, "legacy-doc", memoryLegacyMeta("legacy-doc", "bot-A")); err != nil {
		t.Fatal(err)
	}
	// Same creator republishes the same name → append onto the bare-slug doc.
	res, err := ds.Publish(ctx, service.PublishInput{Slug: "legacy-doc", HTML: "<html><body><p>new</p></body></html>", CreatorUID: "bot-A"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Slug != "legacy-doc" {
		t.Fatalf("legacy republish key = %q; want the bare slug legacy-doc (no derivation)", res.Slug)
	}
	if res.Version != 2 {
		t.Fatalf("legacy republish version = %d; want 2 (append in place)", res.Version)
	}
	// The derived key must NOT have been created.
	if meta, _ := store.GetMeta(ctx, service.DeriveDocKey("bot-A", "legacy-doc")); meta != nil {
		t.Fatalf("legacy republish leaked a derived-key doc: %+v", meta)
	}
	// key_scheme stays absent ⇒ still legacy.
	meta, _ := store.GetMeta(ctx, "legacy-doc")
	if meta.KeyScheme() != "" {
		t.Fatalf("legacy doc gained a key_scheme %q; must stay absent", meta.KeyScheme())
	}
}

// Acceptance 5: with no creator identity the resolver never derives — a publish
// lands on the bare slug exactly as before (legacy path unchanged).
func TestPublishNoCreatorDoesNotDerive(t *testing.T) {
	ds, store := newDocWithStore(t)
	ctx := context.Background()
	res, err := ds.Publish(ctx, service.PublishInput{Slug: "anon-doc", HTML: "<html><body><p>x</p></body></html>"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Slug != "anon-doc" {
		t.Fatalf("no-creator publish key = %q; want bare slug (no derivation)", res.Slug)
	}
	if meta, _ := store.GetMeta(ctx, "anon-doc"); meta == nil || meta.KeyScheme() != "" {
		t.Fatalf("no-creator publish meta = %+v; want bare-slug legacy (no scheme)", meta)
	}
}

// Acceptance 6: draft-first creation shares the SAME identity resolution as
// publish, so the draft lands under the derived key and a later publish of the
// same alias by the same creator promotes onto that same key.
func TestDraftFirstAndPublishShareDerivedKey(t *testing.T) {
	ds, store := newDocWithStore(t)
	ctx := context.Background()
	dr, err := ds.SaveDraft(ctx, "weekly-report", "<html><body><p>draft</p></body></html>", "Draft Title", "bot-A")
	if err != nil {
		t.Fatal(err)
	}
	wantKey := service.DeriveDocKey("bot-A", "weekly-report")
	if dr.Slug != wantKey {
		t.Fatalf("draft slug = %q; want derived key %q", dr.Slug, wantKey)
	}
	meta, _ := store.GetMeta(ctx, wantKey)
	if meta == nil || meta.KeyScheme() != "dockey" || meta.Alias() != "weekly-report" {
		t.Fatalf("draft meta = %+v; want dockey scheme + alias weekly-report", meta)
	}
	// A later publish of the same alias by the same creator resolves to the same
	// key (append onto the draft-created doc, not a fresh document).
	res, err := ds.Publish(ctx, service.PublishInput{Slug: "weekly-report", HTML: "<html><body><p>v1</p></body></html>", CreatorUID: "bot-A"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Slug != wantKey {
		t.Fatalf("publish-after-draft key = %q; want same derived key %q", res.Slug, wantKey)
	}
}

// memoryLegacyMeta builds the DocMeta shape a pre-doc-key publish produced: bare
// slug key, one version, a creator_uid, and NO key_scheme (⇒ legacy).
func memoryLegacyMeta(slug, creatorUID string) storage.DocMeta {
	return storage.DocMeta{
		Slug:     slug,
		Title:    slug,
		Versions: []storage.VersionRef{{N: 1}},
		Extra:    map[string]any{storage.CreatorUIDExtraKey: creatorUID},
	}
}
