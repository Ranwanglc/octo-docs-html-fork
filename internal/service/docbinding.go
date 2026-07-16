package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DocBinding is FEAT-3's per-slug capability probe: given the caller's octo
// token + slug, ask octo-server whether this uid can see the binding and, if
// so, whether they created it. The doc-side `bestCred` seam maps the answer
// onto CapReader / CapAuthor so a non-superAdmin octo user gets scoped access
// without a share code.
//
// Contract notes (matches octo-server modules/doc_binding/api.go):
//   - Wire: GET <base>/v1/docs/bindings/<slug> with Authorization: Bearer *** //   - JSON: `{"data": {"slug","mount_type","group_no?","thread_id?","space_id?",
//     "creator_uid","allow_share_code"}}`. Any other body shape ⇒ (nil, error).
//   - hidden-404: octo-server returns 404 for non-members (does not confirm
//     the slug exists). We map that to (nil, nil) so the caller falls through
//     to the next credential — same "no cap here" contract as the bridge.
//   - Every other non-2xx (401/403/5xx) ⇒ (nil, error) and the caller logs +
//     falls through. Never a 500 into the request path.
//
// Author vs reader mapping (see bestCred call site): the endpoint currently
// reports only `creator_uid`, not a per-caller write bit — so we treat
// `creator_uid == uid` as CapAuthor (binding creator, per canWrite thread-owner
// rule) and any other visible response as CapReader. Group/space managers who
// are not the binding creator still land as reader on this path; that gap is
// acceptable for FEAT-3/A and can be tightened when FEAT-2 exposes an explicit
// `can_write` flag.

// DocBindingInfo mirrors the octo-server bindingResp fields the doc side needs.
type DocBindingInfo struct {
	Slug           string `json:"slug"`
	MountType      string `json:"mount_type"`
	GroupNo        string `json:"group_no,omitempty"`
	ThreadId       string `json:"thread_id,omitempty"`
	SpaceId        string `json:"space_id,omitempty"`
	CreatorUID     string `json:"creator_uid"`
	AllowShareCode bool   `json:"allow_share_code"`
}

// BindingFetcher fetches a binding for (token, slug). Injectable so tests do
// not need a real octo-server; a nil binding + nil error means hidden-404
// (caller has no cap here), any error means "flaky octo, fall through".
type BindingFetcher interface {
	Fetch(ctx context.Context, token, slug string) (*DocBindingInfo, error)
}

// DocBindingClient ties a fetcher to a TTL cache keyed by (token, slug).
type DocBindingClient struct {
	fetcher BindingFetcher
	ttl     time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]docBindingCacheEntry
}

type docBindingCacheEntry struct {
	info    *DocBindingInfo // nil = hidden-404; cached so repeat probes short-circuit
	expires time.Time
}

// NewDocBindingClient wires a fetcher and cache TTL. ttl<=0 disables caching.
func NewDocBindingClient(fetcher BindingFetcher, ttl time.Duration) *DocBindingClient {
	return &DocBindingClient{
		fetcher: fetcher,
		ttl:     ttl,
		now:     time.Now,
		cache:   make(map[string]docBindingCacheEntry),
	}
}

// Resolve returns the binding for (token, slug), or nil if the caller has no
// cap here (hidden-404 or empty inputs). Nil binding + nil error is the "no
// cap" signal — bestCred treats it as "skip this channel" so the fallback
// chain stays clean. Fetcher errors surface so the caller can log + fall
// through without a 500.
func (c *DocBindingClient) Resolve(ctx context.Context, token, slug string) (*DocBindingInfo, error) {
	token = strings.TrimSpace(token)
	slug = strings.TrimSpace(slug)
	if c == nil || c.fetcher == nil || token == "" || slug == "" {
		return nil, nil
	}
	// NUL is disallowed in HTTP header values and in slugs, so it is a safe
	// separator that cannot be forged into a collision between (t1, s2) and
	// (t1s, 2).
	key := token + "\x00" + slug
	if info, hit := c.cacheGet(key); hit {
		return info, nil
	}
	info, err := c.fetcher.Fetch(ctx, token, slug)
	if err != nil {
		return nil, err
	}
	// Cache both hits and hidden-404s: a revoked/absent binding must not
	// hammer octo for every request, and the TTL bounds staleness the same
	// way it does for userinfo.
	c.cachePut(key, info)
	return info, nil
}

func (c *DocBindingClient) cacheGet(key string) (*DocBindingInfo, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.cache[key]; ok && c.now().Before(e.expires) {
		return e.info, true
	}
	return nil, false
}

func (c *DocBindingClient) cachePut(key string, info *DocBindingInfo) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[key] = docBindingCacheEntry{info: info, expires: c.now().Add(c.ttl)}
}

// HTTPBindingFetcher is the default fetcher: one GET per (token, slug), Bearer
// auth, bounded timeout. Base URL is octo-server's origin — the /v1/docs/bindings
// suffix is appended here so callers configure only OCTO_DOC_BINDING_URL.
type HTTPBindingFetcher struct {
	BaseURL string
	Client  *http.Client
}

// NewHTTPBindingFetcher builds the default fetcher. timeout<=0 defaults to 3s
// so a slow octo cannot block a doc request longer than a request cycle.
func NewHTTPBindingFetcher(baseURL string, timeout time.Duration) *HTTPBindingFetcher {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &HTTPBindingFetcher{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Client:  &http.Client{Timeout: timeout},
	}
}

// Fetch calls octo-server's binding endpoint. hidden-404 → (nil, nil).
func (f *HTTPBindingFetcher) Fetch(ctx context.Context, token, slug string) (*DocBindingInfo, error) {
	if f == nil || f.BaseURL == "" {
		return nil, errors.New("doc_binding URL not configured")
	}
	// url.PathEscape guards against a hostile slug (though upstream validation
	// is [A-Za-z0-9_-] and thus a no-op today) so a future slug-rule change
	// never turns into a path-injection bug.
	endpoint := f.BaseURL + "/v1/docs/bindings/" + url.PathEscape(slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		// hidden-404: octo-server does not confirm the slug exists to a
		// non-member. Map to "no cap here" so the fallback chain continues.
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("doc_binding: status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	// octo-server wraps handlers in `{data: ...}` envelopes; accept either
	// that shape or a bare object so a response-shape tweak upstream stays
	// forward-compatible.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("doc_binding: read: %w", err)
	}
	var env struct {
		Data *DocBindingInfo `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Data != nil && env.Data.Slug != "" {
		return env.Data, nil
	}
	var bare DocBindingInfo
	if err := json.Unmarshal(body, &bare); err != nil {
		return nil, fmt.Errorf("doc_binding: decode: %w", err)
	}
	if bare.Slug == "" {
		return nil, errors.New("doc_binding: response missing slug")
	}
	return &bare, nil
}
