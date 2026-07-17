package octoidentity_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/octoidentity"
)

// stub server: routes verify/user based on path; per-case handler injected via
// map keyed by method+path so a single test server covers all branches.
func newStub(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for k, h := range routes {
		parts := strings.SplitN(k, " ", 2)
		wantMethod, path := parts[0], parts[1]
		h := h
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != wantMethod {
				http.Error(w, "bad method", http.StatusMethodNotAllowed)
				return
			}
			h(w, r)
		})
	}
	return httptest.NewServer(mux)
}

func TestVerifyTokenOK(t *testing.T) {
	srv := newStub(t, map[string]http.HandlerFunc{
		"POST /v1/auth/verify": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"token":"tok-abc"`) {
				t.Errorf("body = %s", body)
			}
			_, _ = io.WriteString(w, `{"uid":"u1","name":"Alice","role":"superAdmin"}`)
		},
	})
	defer srv.Close()

	id := octoidentity.New(srv.URL, "", time.Second)
	u, err := id.VerifyToken(context.Background(), "tok-abc")
	if err != nil {
		t.Fatal(err)
	}
	if u == nil || u.UID != "u1" || u.Name != "Alice" || u.Role != "superAdmin" {
		t.Fatalf("user = %+v", u)
	}
}

func TestVerifyTokenNon2xxReturnsNil(t *testing.T) {
	srv := newStub(t, map[string]http.HandlerFunc{
		"POST /v1/auth/verify": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusUnauthorized)
		},
	})
	defer srv.Close()
	id := octoidentity.New(srv.URL, "", time.Second)
	u, err := id.VerifyToken(context.Background(), "x")
	if err != nil || u != nil {
		t.Fatalf("expected (nil, nil), got %+v, %v", u, err)
	}
}

func TestVerifyTokenMissingUIDReturnsNil(t *testing.T) {
	srv := newStub(t, map[string]http.HandlerFunc{
		"POST /v1/auth/verify": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"name":"nobody"}`)
		},
	})
	defer srv.Close()
	id := octoidentity.New(srv.URL, "", time.Second)
	u, _ := id.VerifyToken(context.Background(), "x")
	if u != nil {
		t.Fatalf("body without uid must yield nil, got %+v", u)
	}
}

func TestVerifyBotOK(t *testing.T) {
	srv := newStub(t, map[string]http.HandlerFunc{
		"POST /v1/auth/verify-bot": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"bot_token":"bot-abc"`) {
				t.Errorf("body = %s", body)
			}
			_, _ = io.WriteString(w, `{"bot_uid":"b1","bot_name":"Build Bot","owner_uid":"u1","space_id":"s1"}`)
		},
	})
	defer srv.Close()

	id := octoidentity.New(srv.URL, "", time.Second)
	bi, err := id.VerifyBot(context.Background(), "bot-abc")
	if err != nil {
		t.Fatal(err)
	}
	if bi == nil || bi.UID != "b1" || bi.Name != "Build Bot" || bi.SpaceID != "s1" || bi.OwnerUID != "u1" {
		t.Fatalf("bot identity = %+v", bi)
	}
}

func TestVerifyBotEmptySpaceReturnsNil(t *testing.T) {
	srv := newStub(t, map[string]http.HandlerFunc{
		"POST /v1/auth/verify-bot": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"bot_uid":"b1","bot_name":"Build Bot","space_id":""}`)
		},
	})
	defer srv.Close()

	id := octoidentity.New(srv.URL, "", time.Second)
	bi, err := id.VerifyBot(context.Background(), "bot-abc")
	if err != nil || bi != nil {
		t.Fatalf("empty space_id = (%+v, %v); want nil, nil", bi, err)
	}
}

func TestVerifyBotWhitespaceUIDReturnsNil(t *testing.T) {
	srv := newStub(t, map[string]http.HandlerFunc{
		"POST /v1/auth/verify-bot": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"bot_uid":"   ","space_id":"s1"}`)
		},
	})
	defer srv.Close()

	id := octoidentity.New(srv.URL, "", time.Second)
	bi, err := id.VerifyBot(context.Background(), "bot-abc")
	if err != nil || bi != nil {
		t.Fatalf("whitespace bot_uid = (%+v, %v); want nil, nil", bi, err)
	}
}

func TestVerifyBotEmptyUIDReturnsNil(t *testing.T) {
	srv := newStub(t, map[string]http.HandlerFunc{
		"POST /v1/auth/verify-bot": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"bot_name":"Build Bot","space_id":"s1"}`)
		},
	})
	defer srv.Close()

	id := octoidentity.New(srv.URL, "", time.Second)
	bi, err := id.VerifyBot(context.Background(), "bot-abc")
	if err != nil || bi != nil {
		t.Fatalf("empty bot_uid = (%+v, %v); want nil, nil", bi, err)
	}
}

func TestVerifyBotWhitespaceSpaceReturnsNil(t *testing.T) {
	srv := newStub(t, map[string]http.HandlerFunc{
		"POST /v1/auth/verify-bot": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"bot_uid":"b1","space_id":"   "}`)
		},
	})
	defer srv.Close()

	id := octoidentity.New(srv.URL, "", time.Second)
	bi, err := id.VerifyBot(context.Background(), "bot-abc")
	if err != nil || bi != nil {
		t.Fatalf("whitespace space_id = (%+v, %v); want nil, nil", bi, err)
	}
}

func TestVerifyBotNon2xxReturnsNil(t *testing.T) {
	srv := newStub(t, map[string]http.HandlerFunc{
		"POST /v1/auth/verify-bot": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusUnauthorized)
		},
	})
	defer srv.Close()

	id := octoidentity.New(srv.URL, "", time.Second)
	bi, err := id.VerifyBot(context.Background(), "bot-abc")
	if err != nil || bi != nil {
		t.Fatalf("401 = (%+v, %v); want nil, nil", bi, err)
	}
}

func TestVerifyTokenEmptyTokenShortCircuits(t *testing.T) {
	// Server would panic (never called) — proves the empty-token guard runs
	// before any network I/O.
	id := octoidentity.New("http://127.0.0.1:1", "", time.Second)
	u, err := id.VerifyToken(context.Background(), "")
	if err != nil || u != nil {
		t.Fatalf("empty token = (%+v, %v); want nil, nil", u, err)
	}
}

func TestVerifyTokenUpstreamUnreachableReturnsNil(t *testing.T) {
	// Port 1 is guaranteed unbound in userspace containers → dial error.
	id := octoidentity.New("http://127.0.0.1:1", "", 100*time.Millisecond)
	u, err := id.VerifyToken(context.Background(), "tok")
	if err != nil || u != nil {
		t.Fatalf("unreachable = (%+v, %v); want nil, nil", u, err)
	}
}

func TestGetUserPrefersServiceToken(t *testing.T) {
	seen := ""
	srv := newStub(t, map[string]http.HandlerFunc{
		"GET /v1/users/u9": func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("token")
			_, _ = io.WriteString(w, `{"uid":"u9","name":"Bob","avatar":"x.png","role":"member"}`)
		},
	})
	defer srv.Close()

	id := octoidentity.New(srv.URL, "svc-tok", time.Second)
	u, err := id.GetUser(context.Background(), "u9", "caller-tok")
	if err != nil || u == nil || u.UID != "u9" || u.Avatar != "x.png" {
		t.Fatalf("user = %+v, err = %v", u, err)
	}
	if seen != "svc-tok" {
		t.Fatalf("service token not sent: got %q", seen)
	}
}

func TestGetUserFallsBackToCallerToken(t *testing.T) {
	seen := ""
	srv := newStub(t, map[string]http.HandlerFunc{
		"GET /v1/users/u9": func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("token")
			_, _ = io.WriteString(w, `{"uid":"u9"}`)
		},
	})
	defer srv.Close()

	id := octoidentity.New(srv.URL, "", time.Second)
	_, _ = id.GetUser(context.Background(), "u9", "caller-tok")
	if seen != "caller-tok" {
		t.Fatalf("caller token not forwarded: %q", seen)
	}
}

func TestGetUserNotFoundReturnsNil(t *testing.T) {
	srv := newStub(t, map[string]http.HandlerFunc{
		"GET /v1/users/u9": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusNotFound)
		},
	})
	defer srv.Close()
	id := octoidentity.New(srv.URL, "", time.Second)
	u, err := id.GetUser(context.Background(), "u9", "")
	if err != nil || u != nil {
		t.Fatalf("404 = (%+v, %v); want nil, nil", u, err)
	}
}

func TestSetGetProviderSeam(t *testing.T) {
	octoidentity.Set(nil)
	if _, err := octoidentity.Get(); err != octoidentity.ErrDisabled {
		t.Fatalf("nil provider must yield ErrDisabled, got %v", err)
	}

	stub := stubIdentity{uid: "u1"}
	octoidentity.Set(stub)
	t.Cleanup(func() { octoidentity.Set(nil) })
	p, err := octoidentity.Get()
	if err != nil || p == nil {
		t.Fatalf("Get after Set: %v, %v", p, err)
	}
	u, _ := p.VerifyToken(context.Background(), "x")
	if u == nil || u.UID != "u1" {
		t.Fatalf("stub round-trip: %+v", u)
	}
}

type stubIdentity struct{ uid string }

func (s stubIdentity) VerifyToken(_ context.Context, _ string) (*octoidentity.User, error) {
	return &octoidentity.User{UID: s.uid}, nil
}
func (s stubIdentity) VerifyBot(_ context.Context, _ string) (*octoidentity.BotIdentity, error) {
	return &octoidentity.BotIdentity{UID: s.uid, SpaceID: "s1"}, nil
}
func (s stubIdentity) GetUser(_ context.Context, uid, _ string) (*octoidentity.User, error) {
	return &octoidentity.User{UID: uid}, nil
}

// captureWarns swaps slog.Default() for a JSON handler over a byte buffer so
// tests can substring-assert on warn output. Restores the original on cleanup.
// Level is set to Warn — Info/Debug would flood the buffer without adding
// coverage. No new dep: log/slog is stdlib.
func captureWarns(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestVerifyToken5xxLogsWarn(t *testing.T) {
	buf := captureWarns(t)
	srv := newStub(t, map[string]http.HandlerFunc{
		"POST /v1/auth/verify": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	})
	defer srv.Close()
	id := octoidentity.New(srv.URL, "", time.Second)
	u, err := id.VerifyToken(context.Background(), "tok")
	if err != nil || u != nil {
		t.Fatalf("contract broken: got (%+v, %v); want nil, nil", u, err)
	}
	out := buf.String()
	if !strings.Contains(out, `"op":"verifyToken"`) || !strings.Contains(out, `"status":500`) {
		t.Fatalf("missing warn fields: %s", out)
	}
	// token must never leak to logs (parity with TS "Never logged" note)
	if strings.Contains(out, "tok") {
		t.Fatalf("token leaked to warn log: %s", out)
	}
}

func TestVerifyTokenTransportErrorLogsWarn(t *testing.T) {
	buf := captureWarns(t)
	// port 1 → immediate dial refusal (same trick as the existing unreachable test)
	id := octoidentity.New("http://127.0.0.1:1", "", 100*time.Millisecond)
	u, err := id.VerifyToken(context.Background(), "tok")
	if err != nil || u != nil {
		t.Fatalf("contract broken: got (%+v, %v); want nil, nil", u, err)
	}
	out := buf.String()
	if !strings.Contains(out, `"op":"verifyToken"`) || !strings.Contains(out, `"err":`) {
		t.Fatalf("missing warn fields: %s", out)
	}
}

func TestVerifyTokenBadJSONLogsWarn(t *testing.T) {
	buf := captureWarns(t)
	srv := newStub(t, map[string]http.HandlerFunc{
		"POST /v1/auth/verify": func(w http.ResponseWriter, _ *http.Request) {
			// 200 header, garbage body → json.Decode fails
			_, _ = io.WriteString(w, "not json {{{")
		},
	})
	defer srv.Close()
	id := octoidentity.New(srv.URL, "", time.Second)
	u, err := id.VerifyToken(context.Background(), "tok")
	if err != nil || u != nil {
		t.Fatalf("contract broken: got (%+v, %v); want nil, nil", u, err)
	}
	out := buf.String()
	if !strings.Contains(out, `"op":"verifyToken"`) || !strings.Contains(out, `"err":`) {
		t.Fatalf("missing warn fields: %s", out)
	}
}

func TestGetUser5xxLogsWarn(t *testing.T) {
	buf := captureWarns(t)
	srv := newStub(t, map[string]http.HandlerFunc{
		"GET /v1/users/u9": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusBadGateway)
		},
	})
	defer srv.Close()
	id := octoidentity.New(srv.URL, "", time.Second)
	u, err := id.GetUser(context.Background(), "u9", "")
	if err != nil || u != nil {
		t.Fatalf("contract broken: got (%+v, %v); want nil, nil", u, err)
	}
	out := buf.String()
	if !strings.Contains(out, `"op":"getUser"`) || !strings.Contains(out, `"status":502`) {
		t.Fatalf("missing warn fields: %s", out)
	}
}
