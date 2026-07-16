package eventwebhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient wires a Client aimed at the given handler. Timeout is short so
// a slow-mock test (no goroutine leak) still finishes fast.
func newTestClient(t *testing.T, h http.Handler, token string) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	// newWithTimeout so a stalled handler in a test can't stretch the whole
	// suite to the production 5s deadline. Production wiring uses New.
	c := newWithTimeout(srv.URL, token, 500*time.Millisecond, nil)
	if c == nil {
		t.Fatalf("newWithTimeout returned nil for non-empty url")
	}
	return c
}

func TestNewReturnsNilForEmptyURL(t *testing.T) {
	// Contract: unset URL ⇒ nil client. CommentService relies on this so it
	// can guard a single s.notify==nil check rather than a URL string.
	c := New("", "tok", nil)
	if c != nil {
		t.Fatalf("expected nil client for empty url, got %#v", c)
	}
	// Whitespace-only should count as unset.
	if got := New("   ", "tok", nil); got != nil {
		t.Fatalf("whitespace-only url should be treated as unset")
	}
}

func TestFireSyncPostsExpectedShape(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotCT     string
		gotToken  string
		gotBody   []byte
	)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotToken = r.Header.Get("X-Octo-Doc-Webhook-Token")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	c := newTestClient(t, h, "secret-token")

	ev := Event{
		EventType: EventTypeCommentCreated,
		Slug:      "my-doc-slug",
		Actor:     Actor{UID: "u123", Name: "张三"},
		Doc:       Doc{Title: "季度复盘", URL: "https://docs.example.com/d/my-doc-slug#c-42"},
		Comment:   Comment{ID: "c-42", Text: "第 3 节评论内容原文", CreatedAt: "2026-07-10T12:34:56Z"},
	}
	if err := c.FireSync(ev); err != nil {
		t.Fatalf("FireSync: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/" {
		t.Errorf("path = %q, want /", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotToken != "secret-token" {
		t.Errorf("X-Octo-Doc-Webhook-Token = %q, want secret-token", gotToken)
	}
	// Verify wire body matches the OCT-137/A payload contract: parse the JSON
	// back so field ordering doesn't matter, and check every documented key.
	var round Event
	if err := json.Unmarshal(gotBody, &round); err != nil {
		t.Fatalf("body not valid JSON: %v (body=%s)", err, gotBody)
	}
	if round != ev {
		t.Errorf("payload mismatch:\n got  %#v\n want %#v", round, ev)
	}
	// Text must be sent verbatim (no HTML escape); assert the raw bytes.
	if !strings.Contains(string(gotBody), `"第 3 节评论内容原文"`) {
		t.Errorf("comment.text should be verbatim, got body=%s", gotBody)
	}
	// doc.url fragment must equal comment.id byte-for-byte — server slices
	// after "#" and matches payload.comment.id (B1).
	if want := "https://docs.example.com/d/my-doc-slug#" + round.Comment.ID; round.Doc.URL != want {
		t.Errorf("doc.url = %q, want %q (fragment must equal comment.id)", round.Doc.URL, want)
	}
}

func TestFireSync5xxReturnsError(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := newTestClient(t, h, "tok")
	err := c.FireSync(Event{EventType: EventTypeCommentCreated, Slug: "s"})
	if err == nil {
		t.Fatal("expected error on 5xx, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should carry status code, got %v", err)
	}
}

func TestFireSyncTransportError(t *testing.T) {
	// Point at 127.0.0.1:1 (unlikely to be listening); the dial will fail fast
	// and FireSync must surface the error rather than hang. This proves the
	// transport-error branch without a slow handler that stalls httptest.Close.
	// newWithTimeout so the failure lands under 500ms instead of the 5s spec.
	c := newWithTimeout("http://127.0.0.1:1/", "tok", 500*time.Millisecond, nil)
	if c == nil {
		t.Fatal("newWithTimeout returned nil")
	}
	done := make(chan error, 1)
	go func() { done <- c.FireSync(Event{EventType: EventTypeCommentCreated, Slug: "s"}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected transport error, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FireSync hung past client Timeout")
	}
}

func TestFireIsAsyncAndSurvivesFailure(t *testing.T) {
	// Fire must never block on the caller, even if the server hangs. Handler
	// pins itself on <-release; if Fire were synchronous it could not return
	// until we close(release), so a Fire-returns-quickly assertion proves
	// non-blocking. The 502 status also proves survives-failure: the notifier
	// logs and drops, never panics up the stack.
	release := make(chan struct{})
	done := make(chan struct{}, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusBadGateway)
		done <- struct{}{}
	})
	c := newTestClient(t, h, "tok")

	start := time.Now()
	c.Fire(t.Context(), Event{EventType: EventTypeCommentCreated, Slug: "s"})
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		close(release) // let the handler go so httptest.Close can shut down.
		t.Fatalf("Fire blocked caller for %v; must return immediately", elapsed)
	}

	// Now release the handler and confirm the goroutine actually did the POST.
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Fire never reached the server after release")
	}
}

// TestTimeoutIsFixed5s pins the spec-mandated timeout so future refactors
// can't accidentally re-plumb a config knob in and stretch detached
// goroutines past 5s (see B2).
func TestTimeoutIsFixed5s(t *testing.T) {
	c := New("http://example.invalid/", "tok", nil)
	if c == nil {
		t.Fatal("New returned nil")
	}
	if got := c.http.Timeout; got != defaultTimeout {
		t.Fatalf("http.Client.Timeout = %v, want %v (spec-fixed 5s)", got, defaultTimeout)
	}
	if defaultTimeout != 5*time.Second {
		t.Fatalf("defaultTimeout drift: got %v want 5s", defaultTimeout)
	}
}

func TestNilClientFireIsNoop(t *testing.T) {
	// Wire-time contract: a nil *Client must be safe to call. CommentService
	// uses this to skip the whole notify branch when the URL is unset.
	var c *Client
	c.Fire(t.Context(), Event{EventType: EventTypeCommentCreated, Slug: "s"})
	if err := c.FireSync(Event{EventType: EventTypeCommentCreated}); err == nil {
		t.Fatal("FireSync on nil client should error, got nil")
	}
}
