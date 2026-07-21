package httpx_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/octoidentity"
)

// creatorStub is the minimal Identity used to prove the render path resolves
// CreatorUID through octoidentity and stamps the returned Name/Avatar into
// __ODOC__ as creator_name / creator_avatar. VerifyToken/VerifyBot go
// unimplemented — no auth is exercised in these tests.
//
// 上游 URL 拼装契约 (is_upload_avatar + avatar_version → /v1/users/:uid/avatar?v=N)
// 由 octoidentity_test.go 覆盖; 本文件只验证 Identity 给的 Avatar URL 能透传进 __ODOC__.
type creatorStub struct {
	name       string
	avatar     string
	calls      int32
	returnNil  bool // simulate 404/upstream fault
	wantCaller string
	gotCaller  string
}

func (c *creatorStub) VerifyToken(_ context.Context, _ string) (*octoidentity.User, error) {
	return nil, nil
}
func (c *creatorStub) VerifyBot(_ context.Context, _ string) (*octoidentity.BotIdentity, error) {
	return nil, nil
}
func (c *creatorStub) GetUser(_ context.Context, uid, callerToken string) (*octoidentity.User, error) {
	atomic.AddInt32(&c.calls, 1)
	if c.wantCaller != "" {
		c.gotCaller = callerToken
	}
	if c.returnNil {
		return nil, nil
	}
	return &octoidentity.User{UID: uid, Name: c.name, Avatar: c.avatar}, nil
}

// TestRenderInjectsCreatorNameAvatar covers OCT-187: the render handler resolves
// the stamped CreatorUID through the octoidentity provider and surfaces name +
// avatar in window.__ODOC__ so the overlay DocMoreMenu can show them instead of
// the raw uid.
func TestRenderInjectsCreatorNameAvatar(t *testing.T) {
	stub := &creatorStub{name: "Alice Bot", avatar: "https://cdn.example/a.png"}
	withStubIdentity(t, stub)

	h := newTestServer(t, nil)
	// Publish stamps CreatorUID = testUID (see server_test.go).
	rec := do(t, h, http.MethodPost, "/v1/docs", authorHdr(),
		`{"slug":"cn","version":1,"html":"<html><body><h1>x</h1></body></html>","meta":{"title":"CN"}}`)
	if rec.Code != 200 {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}

	body := do(t, h, http.MethodGet, "/d/cn/v/1", authorHdrNoCT(), "").Body.String()
	if !strings.Contains(body, `"creator_uid":"`+testUID+`"`) {
		t.Fatalf("creator_uid missing from __ODOC__: %s", body)
	}
	if !strings.Contains(body, `"creator_name":"Alice Bot"`) {
		t.Errorf("creator_name missing from __ODOC__: %s", body)
	}
	if !strings.Contains(body, `"creator_avatar":"https://cdn.example/a.png"`) {
		t.Errorf("creator_avatar missing from __ODOC__: %s", body)
	}
}

// TestRenderCreatorMissProviderSoftFail: provider returns (nil, nil) → both
// creator_name and creator_avatar are omitted (omitempty), creator_uid still
// surfaces, and the render succeeds. Same contract for provider-disabled.
func TestRenderCreatorMissProviderSoftFail(t *testing.T) {
	// (a) provider present but returns nil.
	stub := &creatorStub{returnNil: true}
	withStubIdentity(t, stub)

	h := newTestServer(t, nil)
	_ = do(t, h, http.MethodPost, "/v1/docs", authorHdr(),
		`{"slug":"nil","version":1,"html":"<html><body><h1>x</h1></body></html>"}`)
	body := do(t, h, http.MethodGet, "/d/nil/v/1", authorHdrNoCT(), "").Body.String()
	if !strings.Contains(body, `"creator_uid":"`+testUID+`"`) {
		t.Fatalf("creator_uid must still surface on provider miss: %s", body)
	}
	if strings.Contains(body, "creator_name") || strings.Contains(body, "creator_avatar") {
		t.Errorf("empty name/avatar must be omitted on provider miss: %s", body)
	}

	// (b) no provider at all.
	octoidentity.Set(nil)
	h2 := newTestServer(t, nil)
	_ = do(t, h2, http.MethodPost, "/v1/docs", authorHdr(),
		`{"slug":"off","version":1,"html":"<html><body><h1>x</h1></body></html>"}`)
	body2 := do(t, h2, http.MethodGet, "/d/off/v/1", authorHdrNoCT(), "").Body.String()
	if strings.Contains(body2, "creator_name") || strings.Contains(body2, "creator_avatar") {
		t.Errorf("provider-off must omit creator_name/creator_avatar: %s", body2)
	}
}

// TestRenderCreatorLookupCached: two renders of the same slug hit the identity
// provider exactly once — the CachingIdentity wrapper installed in production
// wiring is exercised here directly to prove the hot render path is short-
// circuited.
func TestRenderCreatorLookupCached(t *testing.T) {
	inner := &creatorStub{name: "Cached Bot", avatar: "https://cdn.example/c.png"}
	cached := octoidentity.NewCachingIdentity(inner, 60_000_000_000 /* 60s */, 16)
	withStubIdentity(t, cached)

	h := newTestServer(t, nil)
	_ = do(t, h, http.MethodPost, "/v1/docs", authorHdr(),
		`{"slug":"cache","version":1,"html":"<html><body><h1>x</h1></body></html>"}`)
	for i := 0; i < 3; i++ {
		body := do(t, h, http.MethodGet, "/d/cache/v/1", authorHdrNoCT(), "").Body.String()
		if !strings.Contains(body, `"creator_name":"Cached Bot"`) {
			t.Fatalf("render %d missing creator_name: %s", i, body)
		}
	}
	if got := atomic.LoadInt32(&inner.calls); got != 1 {
		t.Errorf("cached identity should be called exactly once across 3 renders, got %d", got)
	}
}
