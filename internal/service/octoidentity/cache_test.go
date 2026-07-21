package octoidentity_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/octoidentity"
)

// countingInner is a stub Identity that records GetUser calls per uid so cache
// hits vs. upstream fetches can be asserted precisely.
type countingInner struct {
	mu        sync.Mutex
	calls     map[string]int
	returnNil bool
	returnErr error
	nameByUID func(string) string
}

func newCountingInner() *countingInner {
	return &countingInner{calls: map[string]int{}}
}

func (c *countingInner) VerifyToken(_ context.Context, _ string) (*octoidentity.User, error) {
	return nil, nil
}
func (c *countingInner) VerifyBot(_ context.Context, _ string) (*octoidentity.BotIdentity, error) {
	return nil, nil
}
func (c *countingInner) GetUser(_ context.Context, uid, _ string) (*octoidentity.User, error) {
	c.mu.Lock()
	c.calls[uid]++
	c.mu.Unlock()
	if c.returnErr != nil {
		return nil, c.returnErr
	}
	if c.returnNil {
		return nil, nil
	}
	name := uid + "-name"
	if c.nameByUID != nil {
		name = c.nameByUID(uid)
	}
	return &octoidentity.User{UID: uid, Name: name, Avatar: uid + "-avatar"}, nil
}
func (c *countingInner) callsFor(uid string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[uid]
}

// TestCachingIdentityHitsInnerOnce: identical uid resolved N times ⇒ inner is
// called exactly once. This is the whole point of the wrapper on the render
// hot path.
func TestCachingIdentityHitsInnerOnce(t *testing.T) {
	inner := newCountingInner()
	c := octoidentity.NewCachingIdentity(inner, time.Minute, 8)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		u, err := c.GetUser(ctx, "u1", "tok")
		if err != nil || u == nil || u.Name != "u1-name" {
			t.Fatalf("get u1 iter %d: user=%+v err=%v", i, u, err)
		}
	}
	if got := inner.callsFor("u1"); got != 1 {
		t.Fatalf("expected 1 inner call, got %d", got)
	}
}

// TestCachingIdentityDoesNotCacheMiss: (nil, nil) from inner must NOT be cached
// — otherwise a transient upstream fault would freeze a uid into a negative for
// the whole TTL and the frontend would keep rendering the raw uid even after
// recovery. Every miss re-asks.
func TestCachingIdentityDoesNotCacheMiss(t *testing.T) {
	inner := newCountingInner()
	inner.returnNil = true
	c := octoidentity.NewCachingIdentity(inner, time.Minute, 8)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		u, _ := c.GetUser(ctx, "u404", "tok")
		if u != nil {
			t.Fatalf("iter %d expected nil, got %+v", i, u)
		}
	}
	if got := inner.callsFor("u404"); got != 3 {
		t.Fatalf("miss must not cache; expected 3 inner calls, got %d", got)
	}
}

// TestCachingIdentityDoesNotCacheError: same soft-fail rationale for a real
// error (which shouldn't happen for GetUser under the nil-nil contract, but the
// wrapper should not paper over one either).
func TestCachingIdentityDoesNotCacheError(t *testing.T) {
	inner := newCountingInner()
	inner.returnErr = errors.New("boom")
	c := octoidentity.NewCachingIdentity(inner, time.Minute, 8)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := c.GetUser(ctx, "uerr", "tok"); err == nil {
			t.Fatalf("iter %d expected error", i)
		}
	}
	if got := inner.callsFor("uerr"); got != 3 {
		t.Fatalf("error must not cache; expected 3 inner calls, got %d", got)
	}
}

// TestCachingIdentityTTLExpiry: after the TTL elapses a fresh call goes back to
// inner. The 20ms TTL keeps the test fast while still exercising the expiry
// branch (get finds the entry and drops it as expired).
func TestCachingIdentityTTLExpiry(t *testing.T) {
	inner := newCountingInner()
	c := octoidentity.NewCachingIdentity(inner, 20*time.Millisecond, 8)
	ctx := context.Background()
	if _, err := c.GetUser(ctx, "u1", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := c.GetUser(ctx, "u1", ""); err != nil {
		t.Fatal(err)
	}
	if got := inner.callsFor("u1"); got != 2 {
		t.Fatalf("expected 2 inner calls across TTL boundary, got %d", got)
	}
}

// TestCachingIdentityLRUEviction: with max=2, inserting a 3rd distinct uid must
// evict the least-recently-used entry. u2 stays hot, u1 gets evicted, so a
// re-lookup of u1 re-hits inner but u2 does not.
func TestCachingIdentityLRUEviction(t *testing.T) {
	inner := newCountingInner()
	c := octoidentity.NewCachingIdentity(inner, time.Minute, 2)
	ctx := context.Background()
	_, _ = c.GetUser(ctx, "u1", "")
	_, _ = c.GetUser(ctx, "u2", "")
	// Touch u2 so u1 is the LRU.
	_, _ = c.GetUser(ctx, "u2", "")
	_, _ = c.GetUser(ctx, "u3", "") // should evict u1
	_, _ = c.GetUser(ctx, "u2", "") // still cached
	_, _ = c.GetUser(ctx, "u1", "") // re-fetch after eviction

	if got := inner.callsFor("u1"); got != 2 {
		t.Errorf("u1 expected 2 calls (initial + re-fetch after eviction), got %d", got)
	}
	if got := inner.callsFor("u2"); got != 1 {
		t.Errorf("u2 expected 1 call (stayed hot), got %d", got)
	}
	if got := inner.callsFor("u3"); got != 1 {
		t.Errorf("u3 expected 1 call, got %d", got)
	}
}

// TestCachingIdentityEmptyUIDBypass: empty uid short-circuits to (nil,nil)
// without touching inner (mirrors HTTPIdentity.GetUser).
func TestCachingIdentityEmptyUIDBypass(t *testing.T) {
	inner := newCountingInner()
	c := octoidentity.NewCachingIdentity(inner, time.Minute, 8)
	u, err := c.GetUser(context.Background(), "", "tok")
	if u != nil || err != nil {
		t.Fatalf("empty uid must return (nil, nil), got %+v %v", u, err)
	}
	if got := inner.callsFor(""); got != 0 {
		t.Fatalf("empty uid must not touch inner, got %d calls", got)
	}
}

// TestCachingIdentityDisabledPassthrough: non-positive ttl or max disables
// caching (wrapper returns inner verbatim) so callers can plug it in
// unconditionally in dev/tests.
func TestCachingIdentityDisabledPassthrough(t *testing.T) {
	inner := newCountingInner()
	if got := octoidentity.NewCachingIdentity(inner, 0, 8); got != inner {
		t.Errorf("ttl=0 must pass through inner unchanged")
	}
	if got := octoidentity.NewCachingIdentity(inner, time.Minute, 0); got != inner {
		t.Errorf("max=0 must pass through inner unchanged")
	}
	if got := octoidentity.NewCachingIdentity(nil, time.Minute, 8); got != nil {
		t.Errorf("nil inner must pass through as nil")
	}
}

// TestCachingIdentityConcurrentSafe: hammer the wrapper from N goroutines to
// prove the LRU internals hold up under contention (the go race detector
// catches missing locks; the miss count sanity-checks correctness).
func TestCachingIdentityConcurrentSafe(t *testing.T) {
	inner := newCountingInner()
	c := octoidentity.NewCachingIdentity(inner, time.Minute, 32)
	var wg sync.WaitGroup
	var ops int64
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < 200; i++ {
				uid := "u" + strconv.Itoa((seed+i)%16)
				if _, err := c.GetUser(ctx, uid, ""); err != nil {
					t.Errorf("get: %v", err)
					return
				}
				atomic.AddInt64(&ops, 1)
			}
		}(g)
	}
	wg.Wait()
	if atomic.LoadInt64(&ops) == 0 {
		t.Fatal("no ops ran")
	}
}
