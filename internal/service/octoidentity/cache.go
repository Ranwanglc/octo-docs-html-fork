package octoidentity

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// CachingIdentity wraps an Identity and memoizes GetUser results in a bounded
// LRU with a short TTL. Render is a hot path — the same publishing-bot uid gets
// resolved on every page hit — so we short-circuit the upstream round-trip. Only
// GetUser is cached (name/avatar can change but tolerate a bit of staleness);
// VerifyToken / VerifyBot are auth calls and pass straight through.
//
// Contract: nil / not-found results are NOT cached, matching the underlying
// nil-nil contract — otherwise a transient upstream fault would freeze into a
// long-lived negative for TTL seconds. Successful lookups with a non-empty UID
// are the only entries that populate the cache.
//
// The cache key is uid only. callerToken is ignored: the same uid resolves to
// the same public identity regardless of who asks; using it as a key would fan
// the cache out per-viewer and defeat the point on a shared render path.
type CachingIdentity struct {
	inner Identity
	ttl   time.Duration
	max   int

	mu    sync.Mutex
	items map[string]*list.Element // uid -> *list.Element{value: *cacheEntry}
	order *list.List               // front = most recently used
}

type cacheEntry struct {
	uid    string
	user   User
	expiry time.Time
}

// NewCachingIdentity wraps inner with an LRU/TTL cache. Non-positive ttl or max
// disable caching (delegates verbatim to inner) so callers don't have to
// special-case tests / dev.
func NewCachingIdentity(inner Identity, ttl time.Duration, max int) Identity {
	if inner == nil || ttl <= 0 || max <= 0 {
		return inner
	}
	return &CachingIdentity{
		inner: inner,
		ttl:   ttl,
		max:   max,
		items: make(map[string]*list.Element, max),
		order: list.New(),
	}
}

// VerifyToken passes through to inner: auth verification is not cached.
func (c *CachingIdentity) VerifyToken(ctx context.Context, token string) (*User, error) {
	return c.inner.VerifyToken(ctx, token)
}

// VerifyBot passes through to inner: bot auth verification is not cached.
func (c *CachingIdentity) VerifyBot(ctx context.Context, botToken string) (*BotIdentity, error) {
	return c.inner.VerifyBot(ctx, botToken)
}

// GetUser hits the cache first; on miss or expiry it asks inner and stores the
// result. A (nil, nil) miss is deliberately not cached — see the type comment.
func (c *CachingIdentity) GetUser(ctx context.Context, uid, callerToken string) (*User, error) {
	if uid == "" {
		return nil, nil
	}
	if u, ok := c.get(uid); ok {
		return u, nil
	}
	u, err := c.inner.GetUser(ctx, uid, callerToken)
	if err != nil || u == nil {
		return u, err
	}
	c.put(uid, *u)
	return u, nil
}

func (c *CachingIdentity) get(uid string) (*User, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[uid]
	if !ok {
		return nil, false
	}
	entry := e.Value.(*cacheEntry)
	if time.Now().After(entry.expiry) {
		// Expired: drop and treat as miss so the next caller refreshes it.
		c.order.Remove(e)
		delete(c.items, uid)
		return nil, false
	}
	c.order.MoveToFront(e)
	u := entry.user
	return &u, true
}

func (c *CachingIdentity) put(uid string, u User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[uid]; ok {
		entry := e.Value.(*cacheEntry)
		entry.user = u
		entry.expiry = time.Now().Add(c.ttl)
		c.order.MoveToFront(e)
		return
	}
	entry := &cacheEntry{uid: uid, user: u, expiry: time.Now().Add(c.ttl)}
	e := c.order.PushFront(entry)
	c.items[uid] = e
	if c.order.Len() > c.max {
		oldest := c.order.Back()
		if oldest != nil {
			c.order.Remove(oldest)
			delete(c.items, oldest.Value.(*cacheEntry).uid)
		}
	}
}
