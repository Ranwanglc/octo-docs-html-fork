// Package eventwebhook posts doc-side comment events to octo-server as a
// best-effort side channel. Failure never affects the user request: notifiers
// are invoked in a detached goroutine, HTTP calls carry a short timeout, and
// non-2xx responses are logged (without the token) then dropped.
//
// Not in scope: retries, queues, dead-lettering, event types beyond
// comment.created. Server-side handles binding lookup and IM fan-out; the doc
// side only knows the slug and shape.
package eventwebhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// EventTypeCommentCreated is the only event type this build emits. Kept as a
// constant so the server-side switch and future event types stay in sync.
const EventTypeCommentCreated = "comment.created"

// defaultTimeout is the HTTP timeout for a doc→server webhook POST. Spec pins
// it at 5s deliberately: this is a detached best-effort side channel, and
// letting global IO_TIMEOUT stretch it would extend the goroutine's grip on
// connections/memory past what the notify path is worth.
const defaultTimeout = 5 * time.Second

// Actor is the (uid, name) shape server needs to render "who commented".
type Actor struct {
	UID  string `json:"uid"`
	Name string `json:"name,omitempty"`
}

// Doc is the doc-context server uses to render "which doc / open where".
type Doc struct {
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
}

// Comment is the comment payload; Text is verbatim — server escapes.
type Comment struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
}

// Event is the on-wire payload; matches the OCT-137/A server contract.
type Event struct {
	EventType string  `json:"event_type"`
	Slug      string  `json:"slug"`
	Actor     Actor   `json:"actor"`
	Doc       Doc     `json:"doc"`
	Comment   Comment `json:"comment"`
}

// Notifier fires a single comment event. Implementations are fire-and-forget:
// they must not block the caller past a Fire call, must not panic out, and
// must never expose the shared secret in logs.
type Notifier interface {
	Fire(ctx context.Context, ev Event)
}

// Client is the default HTTP Notifier. Empty URL ⇒ nil client from New; the
// caller checks nil at wire time.
type Client struct {
	url    string
	token  string
	http   *http.Client
	logger *slog.Logger
}

// New wires a Client. Returns nil when url is empty so callers can pass nil
// through as "webhook disabled" without a branch at every call site. token=""
// still sends: rejection is the server's call. Timeout is fixed at
// defaultTimeout — spec-mandated and not caller-configurable; tests that need
// a shorter deadline reach for newWithTimeout.
func New(url, token string, logger *slog.Logger) *Client {
	return newWithTimeout(url, token, defaultTimeout, logger)
}

// newWithTimeout is the internal constructor tests use to keep httptest
// handlers snappy without exposing a knob production wiring can wire wrong.
// Keep it unexported: production wiring must go through New.
func newWithTimeout(url, token string, timeout time.Duration, logger *slog.Logger) *Client {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		url:    url,
		token:  strings.TrimSpace(token),
		http:   &http.Client{Timeout: timeout},
		logger: logger,
	}
}

// Fire enqueues one event as a detached goroutine. The parent request context
// is intentionally not threaded through — request cancellation must not kill a
// notify in flight, and a slow octo-server must not stretch p99 latency.
func (c *Client) Fire(_ context.Context, ev Event) {
	if c == nil {
		return
	}
	go c.send(ev)
}

func (c *Client) send(ev Event) {
	// send owns its own context so a stalled server can't hold the goroutine
	// past the HTTP timeout.
	ctx, cancel := context.WithTimeout(context.Background(), c.http.Timeout)
	defer cancel()

	buf, err := json.Marshal(ev)
	if err != nil {
		// Marshal only fails on truly weird types; log and drop.
		c.logger.Warn("doc_event_webhook marshal failed",
			"slug", ev.Slug, "event_type", ev.EventType, "err", err.Error())
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(buf))
	if err != nil {
		c.logger.Warn("doc_event_webhook request build failed",
			"slug", ev.Slug, "event_type", ev.EventType, "err", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// Token header carries the shared secret; never log the value.
	req.Header.Set("X-Octo-Doc-Webhook-Token", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		c.logger.Warn("doc_event_webhook post failed",
			"slug", ev.Slug, "event_type", ev.EventType, "err", err.Error())
		return
	}
	defer func() {
		// Drain then close so the connection returns to the keep-alive pool.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.logger.Warn("doc_event_webhook non-2xx",
			"slug", ev.Slug, "event_type", ev.EventType, "http_status", resp.StatusCode)
		return
	}
}

// FireSync posts synchronously and returns the outcome. Tests use it to
// assert delivery; the request path uses Fire.
func (c *Client) FireSync(ev Event) error {
	if c == nil {
		return errors.New("eventwebhook: nil client")
	}
	buf, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.http.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Octo-Doc-Webhook-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("eventwebhook: non-2xx status " + strconv.Itoa(resp.StatusCode))
	}
	return nil
}
