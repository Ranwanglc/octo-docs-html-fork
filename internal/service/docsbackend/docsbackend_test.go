package docsbackend

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestInternalRegisterAndBotDeleteURLs(t *testing.T) {
	requests := make(chan *http.Request, 2)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"docId":"d1","octoDocSlug":"s1","shareUrl":"https://example/d1","created":true}`)
		}
	}))
	defer ts.Close()

	client := newWithTimeout(ts.URL+"/v1/bot/docs", "", "bot", "internal", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := client.Register(context.Background(), Registration{OctoDocSlug: "s1", Internal: true}, ""); err != nil {
		t.Fatal(err)
	}
	client.Delete(context.Background(), "s1", "")
	post, del := <-requests, <-requests
	if post.Method != http.MethodPost || post.URL.Path != "/internal/html/register" {
		t.Fatalf("register = %s %s", post.Method, post.URL.Path)
	}
	if del.Method != http.MethodDelete || del.URL.Path != "/v1/bot/docs/octo-doc/s1" {
		t.Fatalf("delete = %s %s", del.Method, del.URL.Path)
	}
	if post.Header.Get("X-Internal-Token") != "internal" || del.Header.Get("Authorization") != "Bearer bot" {
		t.Fatal("register/delete did not use their distinct authentication channels")
	}
}

func TestClientRejectsEveryRedirect(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := newWithTimeout(origin.URL+"/v1/bot/docs", "", "bot", "internal", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := client.Register(context.Background(), Registration{OctoDocSlug: "s1"}, "")
	if err == nil {
		t.Fatal("redirect response accepted")
	}
	if redirected {
		t.Fatal("client followed redirect")
	}
}

// TestExplicitInternalRegisterURL: when internalRegisterURL is set, the user
// (Internal) publish hits it verbatim (no /v1/bot/docs suffix derivation),
// while every bot-face call (Register non-internal, Rename, Delete) keeps using
// registerURL. Covers the contract's "显式设置" row plus the bot-face-unaffected
// guarantee. Critically, it asserts credentials do NOT cross origins in either
// direction: once the internal endpoint is a separate trusted origin, a header
// mixup is a real secret-exfiltration path, so absence is asserted, not just
// presence.
func TestExplicitInternalRegisterURL(t *testing.T) {
	botReqs := make(chan *http.Request, 4)
	bot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		botReqs <- r.Clone(r.Context())
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"docId":"d1","octoDocSlug":"s1","shareUrl":"https://example/d1","created":true}`)
		}
	}))
	defer bot.Close()
	intReqs := make(chan *http.Request, 1)
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		intReqs <- r.Clone(r.Context())
		_, _ = io.WriteString(w, `{"docId":"d1","octoDocSlug":"s1","shareUrl":"https://example/d1","created":true}`)
	}))
	defer internal.Close()

	// Explicit internal endpoint on a different origin than the bot base, with a
	// non /internal/html/register path to prove it is used verbatim.
	client := newWithTimeout(bot.URL+"/v1/bot/docs", internal.URL+"/custom/register", "bot", "internal", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// User (internal) publish → explicit internal origin, verbatim path.
	if _, err := client.Register(context.Background(), Registration{OctoDocSlug: "s1", Internal: true}, ""); err != nil {
		t.Fatal(err)
	}
	intReq := recvReq(t, intReqs, "internal register")
	if intReq.Method != http.MethodPost || intReq.URL.Path != "/custom/register" {
		t.Fatalf("internal register = %s %s, want POST /custom/register (verbatim, no derivation)", intReq.Method, intReq.URL.Path)
	}
	if intReq.Host != mustHost(t, internal.URL) {
		t.Fatalf("internal register hit host %q, want the explicit internal origin", intReq.Host)
	}
	if intReq.Header.Get("X-Internal-Token") != "internal" {
		t.Fatal("internal register did not use the internal token")
	}
	// Negative: the bot bearer must NOT ride along to the internal origin.
	if _, ok := intReq.Header["Authorization"]; ok {
		t.Fatal("bot bearer leaked onto the internal origin")
	}

	// Bot-face Register (non-internal) → bot origin, bearer only, no internal token.
	if _, err := client.Register(context.Background(), Registration{OctoDocSlug: "s1"}, ""); err != nil {
		t.Fatal(err)
	}
	botReg := recvReq(t, botReqs, "bot register")
	if botReg.Method != http.MethodPost || botReg.URL.Path != "/v1/bot/docs" {
		t.Fatalf("bot register = %s %s, want POST /v1/bot/docs on the bot origin", botReg.Method, botReg.URL.Path)
	}
	if botReg.Host != mustHost(t, bot.URL) {
		t.Fatalf("bot register hit host %q, want the bot origin", botReg.Host)
	}
	if botReg.Header.Get("Authorization") != "Bearer bot" {
		t.Fatal("bot register did not use the bot bearer token")
	}
	if _, ok := botReg.Header["X-Internal-Token"]; ok {
		t.Fatal("internal token leaked onto the bot origin via Register")
	}

	// Bot-face Rename → bot origin, bearer only, no internal token.
	client.Rename(context.Background(), "s1", "new title", "")
	ren := recvReq(t, botReqs, "bot rename")
	if ren.Method != http.MethodPatch || ren.URL.Path != "/v1/bot/docs/octo-doc/s1" {
		t.Fatalf("rename = %s %s, want PATCH /v1/bot/docs/octo-doc/s1 on the bot origin", ren.Method, ren.URL.Path)
	}
	if ren.Host != mustHost(t, bot.URL) {
		t.Fatalf("rename hit host %q, want the bot origin", ren.Host)
	}
	if ren.Header.Get("Authorization") != "Bearer bot" {
		t.Fatal("rename did not use the bot bearer token")
	}
	if _, ok := ren.Header["X-Internal-Token"]; ok {
		t.Fatal("internal token leaked onto the bot origin via Rename")
	}

	// Bot-face Delete → bot origin, bearer only, no internal token.
	client.Delete(context.Background(), "s1", "")
	del := recvReq(t, botReqs, "bot delete")
	if del.Method != http.MethodDelete || del.URL.Path != "/v1/bot/docs/octo-doc/s1" {
		t.Fatalf("delete = %s %s, want DELETE /v1/bot/docs/octo-doc/s1 on the bot origin", del.Method, del.URL.Path)
	}
	if del.Host != mustHost(t, bot.URL) {
		t.Fatalf("delete hit host %q, want the bot origin", del.Host)
	}
	if del.Header.Get("Authorization") != "Bearer bot" {
		t.Fatal("delete did not use the bot bearer token")
	}
	if _, ok := del.Header["X-Internal-Token"]; ok {
		t.Fatal("internal token leaked onto the bot origin via Delete")
	}

	// The internal origin must never have received a bot-face request.
	select {
	case stray := <-intReqs:
		t.Fatalf("internal origin unexpectedly received %s %s; bot face leaked onto the internal origin", stray.Method, stray.URL.Path)
	default:
	}
}

// TestInternalRegisterURLFallbackByteForByte: empty internalRegisterURL keeps
// today's derivation — strip /v1/bot/docs, append /internal/html/register on the
// same origin. Covers the contract's "未设/空/全空格" row.
func TestInternalRegisterURLFallbackByteForByte(t *testing.T) {
	for _, explicit := range []string{"", "   "} {
		reqs := make(chan *http.Request, 1)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs <- r.Clone(r.Context())
			_, _ = io.WriteString(w, `{"docId":"d1","octoDocSlug":"s1","shareUrl":"https://example/d1","created":true}`)
		}))
		client := newWithTimeout(ts.URL+"/v1/bot/docs", explicit, "bot", "internal", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if _, err := client.Register(context.Background(), Registration{OctoDocSlug: "s1", Internal: true}, ""); err != nil {
			ts.Close()
			t.Fatalf("explicit=%q: %v", explicit, err)
		}
		req := recvReq(t, reqs, "derived internal register")
		if req.Method != http.MethodPost || req.URL.Path != "/internal/html/register" {
			ts.Close()
			t.Fatalf("explicit=%q: register = %s %s, want derived POST /internal/html/register", explicit, req.Method, req.URL.Path)
		}
		if req.Host != mustHost(t, ts.URL) {
			ts.Close()
			t.Fatalf("explicit=%q: derived register hit host %q, want same origin as registerURL", explicit, req.Host)
		}
		ts.Close()
	}
}

func mustHost(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Host
}

// recvReq receives one request off the stub channel with a bounded wait so a
// missing/misrouted request fails with a named assertion instead of hanging
// until the whole-test 10-minute panic.
func recvReq(t *testing.T, ch <-chan *http.Request, what string) *http.Request {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: stub never received a request", what)
		return nil
	}
}
