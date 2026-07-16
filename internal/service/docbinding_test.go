package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lml2468/octo-doc/internal/service"
)

// stubBinding is a table-driven fetcher: (token,slug) → info/err. Counts every
// call so cache assertions can pin exact QPS on octo-server.
type stubBinding struct {
	byKey map[string]stubBindingEntry
	calls int
}

type stubBindingEntry struct {
	info *service.DocBindingInfo
	err  error
}

func (s *stubBinding) Fetch(_ context.Context, token, slug string) (*service.DocBindingInfo, error) {
	s.calls++
	if e, ok := s.byKey[token+"|"+slug]; ok {
		return e.info, e.err
	}
	// Absent key defaults to hidden-404 semantics (no cap here) — mirrors the
	// wire contract so unset table rows do not have to spell it out.
	return nil, nil
}

func TestDocBindingResolveHitCachesWithinTTL(t *testing.T) {
	f := &stubBinding{byKey: map[string]stubBindingEntry{
		"tok|s1": {info: &service.DocBindingInfo{Slug: "s1", MountType: "group", CreatorUID: "u-1"}},
	}}
	c := service.NewDocBindingClient(f, time.Minute)
	for i := 0; i < 5; i++ {
		info, err := c.Resolve(context.Background(), "tok", "s1")
		if err != nil || info == nil || info.CreatorUID != "u-1" {
			t.Fatalf("Resolve[%d] = %+v, %v", i, info, err)
		}
	}
	if f.calls != 1 {
		t.Fatalf("cache within TTL: want 1 fetch, got %d", f.calls)
	}
}

func TestDocBindingResolveHiddenNotFoundCachedAsNil(t *testing.T) {
	// Nil-info + nil-err = hidden-404. Must be cached: a non-member hammering
	// the same slug otherwise DoSes octo-server through the doc side.
	f := &stubBinding{byKey: map[string]stubBindingEntry{
		"tok|missing": {info: nil, err: nil},
	}}
	c := service.NewDocBindingClient(f, time.Minute)
	for i := 0; i < 3; i++ {
		info, err := c.Resolve(context.Background(), "tok", "missing")
		if info != nil || err != nil {
			t.Fatalf("hidden-404 Resolve[%d] = %v, %v", i, info, err)
		}
	}
	if f.calls != 1 {
		t.Fatalf("hidden-404 must cache: want 1 fetch, got %d", f.calls)
	}
}

func TestDocBindingResolveExpires(t *testing.T) {
	// TTL boundary: once the entry expires, the next call re-fetches.
	f := &stubBinding{byKey: map[string]stubBindingEntry{
		"tok|s1": {info: &service.DocBindingInfo{Slug: "s1", CreatorUID: "u"}},
	}}
	c := service.NewDocBindingClient(f, 10*time.Millisecond)
	if _, err := c.Resolve(context.Background(), "tok", "s1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := c.Resolve(context.Background(), "tok", "s1"); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Fatalf("expected 2 fetches across TTL boundary, got %d", f.calls)
	}
}

func TestDocBindingResolveEmptyInputsNoop(t *testing.T) {
	// Empty token or slug = "no cap here"; must not hit fetcher. Keeps the
	// fallback chain (share code → 404) clean when the middleware saw no
	// X-Octo-Token but a handler still asks for a per-slug probe.
	f := &stubBinding{byKey: map[string]stubBindingEntry{}}
	c := service.NewDocBindingClient(f, time.Minute)
	for _, tc := range []struct{ token, slug string }{
		{"", "s"}, {" ", "s"}, {"tok", ""}, {"tok", "  "},
	} {
		info, err := c.Resolve(context.Background(), tc.token, tc.slug)
		if info != nil || err != nil {
			t.Fatalf("empty (%q,%q) = %v, %v", tc.token, tc.slug, info, err)
		}
	}
	if f.calls != 0 {
		t.Fatalf("empty inputs must not fetch; calls=%d", f.calls)
	}
}

func TestDocBindingResolveErrorNotCached(t *testing.T) {
	// Transport / 5xx errors must not poison the cache — otherwise a single
	// blip locks out a viewer for the whole TTL window.
	f := &stubBinding{byKey: map[string]stubBindingEntry{
		"tok|s1": {err: errors.New("boom")},
	}}
	c := service.NewDocBindingClient(f, time.Minute)
	for i := 0; i < 3; i++ {
		if _, err := c.Resolve(context.Background(), "tok", "s1"); err == nil {
			t.Fatalf("Resolve[%d] want error", i)
		}
	}
	if f.calls != 3 {
		t.Fatalf("errors must not cache; calls=%d", f.calls)
	}
}

func TestDocBindingResolveNilClientAndNilFetcher(t *testing.T) {
	// Belt-and-braces: nil client and nil fetcher are both "no cap here",
	// never a panic. Matches the FEAT-1 (nil,nil) fallback shape so callers
	// can plumb the same conditional in bestCred.
	if info, err := (*service.DocBindingClient)(nil).Resolve(context.Background(), "t", "s"); info != nil || err != nil {
		t.Fatalf("nil client Resolve = %v, %v", info, err)
	}
	c := service.NewDocBindingClient(nil, time.Minute)
	if info, err := c.Resolve(context.Background(), "t", "s"); info != nil || err != nil {
		t.Fatalf("nil fetcher Resolve = %v, %v", info, err)
	}
}

func TestHTTPBindingFetcherDecodesEnvelope(t *testing.T) {
	// Happy path: octo-server wraps the resource in `{data:{...}}`; the
	// fetcher must unwrap and forward the caller's Bearer verbatim.
	var gotAuth, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"slug": "s1", "mount_type": "group", "group_no": "g1",
				"creator_uid": "u-9", "allow_share_code": true,
			},
		})
	}))
	t.Cleanup(ts.Close)

	f := service.NewHTTPBindingFetcher(ts.URL, time.Second)
	info, err := f.Fetch(context.Background(), "tok-a", "s1")
	if err != nil || info == nil {
		t.Fatalf("Fetch = %v, %v", info, err)
	}
	if info.Slug != "s1" || info.CreatorUID != "u-9" || !info.AllowShareCode {
		t.Fatalf("decoded = %+v", info)
	}
	if gotAuth != "Bearer tok-a" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != "/v1/docs/bindings/s1" {
		t.Errorf("path = %q; want /v1/docs/bindings/s1", gotPath)
	}
}

func TestHTTPBindingFetcher404IsHidden(t *testing.T) {
	// Octo-server's hidden-404 must surface as (nil,nil) — otherwise every
	// non-member request becomes a spurious error in the doc log.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"code":"doc_binding.not_found"}}`, http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)

	f := service.NewHTTPBindingFetcher(ts.URL, time.Second)
	info, err := f.Fetch(context.Background(), "tok", "nope")
	if info != nil || err != nil {
		t.Fatalf("404 = %v, %v; want (nil,nil)", info, err)
	}
}

func TestHTTPBindingFetcherNon2xxErrors(t *testing.T) {
	// 401/403/5xx are real failures — surface them so the caller can log +
	// fall through (never a 500 into the doc request).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	f := service.NewHTTPBindingFetcher(ts.URL, time.Second)
	if _, err := f.Fetch(context.Background(), "tok", "s1"); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestHTTPBindingFetcherAcceptsBareObject(t *testing.T) {
	// Forward-compat: if octo-server ever drops the {data:...} envelope on
	// this endpoint, the fetcher should still decode. Otherwise a wire tweak
	// upstream silently breaks the FEAT-3 channel.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"slug":"s2","mount_type":"space","space_id":"sp1","creator_uid":"u-1"}`))
	}))
	t.Cleanup(ts.Close)

	f := service.NewHTTPBindingFetcher(ts.URL, time.Second)
	info, err := f.Fetch(context.Background(), "tok", "s2")
	if err != nil || info == nil || info.Slug != "s2" || info.SpaceId != "sp1" {
		t.Fatalf("bare-object decode = %+v, %v", info, err)
	}
}

func TestHTTPBindingFetcherURLNotConfigured(t *testing.T) {
	f := service.NewHTTPBindingFetcher("", time.Second)
	if _, err := f.Fetch(context.Background(), "tok", "s"); err == nil {
		t.Fatal("expected error when base URL empty")
	}
}
