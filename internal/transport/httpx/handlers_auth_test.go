package httpx_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/octoidentity"
)

// stubIdentity is a hand-injected octoidentity.Identity for exercising the
// login handler without hitting the network.
type stubIdentity struct {
	uid         string
	name        string
	role        string
	verify      error
	botUID      string
	botName     string
	botSpaceID  string
	botOwnerUID string
}

func (s stubIdentity) VerifyToken(_ context.Context, tok string) (*octoidentity.User, error) {
	if s.verify != nil {
		return nil, s.verify
	}
	if s.uid == "" {
		return nil, nil
	}
	if tok == "" {
		return nil, nil
	}
	return &octoidentity.User{UID: s.uid, Name: s.name, Role: s.role}, nil
}
func (s stubIdentity) VerifyBot(_ context.Context, tok string) (*octoidentity.BotIdentity, error) {
	if tok == "" || s.botUID == "" || s.botSpaceID == "" {
		return nil, nil
	}
	return &octoidentity.BotIdentity{UID: s.botUID, Name: s.botName, SpaceID: s.botSpaceID, OwnerUID: s.botOwnerUID}, nil
}
func (s stubIdentity) GetUser(_ context.Context, uid, _ string) (*octoidentity.User, error) {
	return &octoidentity.User{UID: uid}, nil
}

func withStubIdentity(t *testing.T, stub octoidentity.Identity) {
	t.Helper()
	octoidentity.Set(stub)
	t.Cleanup(func() { octoidentity.Set(nil) })
}

// A provider that flips authConfigured on: this is what the overlay reads to
// decide whether to show the login affordance. Setting OctoServerBaseURL is
// the OCT-150 gate (equivalent to wiring a real provider in prod).
func loginCfg() *config.Config {
	return &config.Config{
		WriteToken: "test-token", MaxHTMLBytes: 5 << 20, RepoURL: "https://x",
		RateLimitMax:      0,
		MaxAssetBytes:     25 << 20,
		AssetMIMEAllow:    []string{"image/png"},
		OctoServerBaseURL: "http://octo.example",
	}
}

func TestLoginRequiresProvider(t *testing.T) {
	// Provider off + login endpoint hit ⇒ 404 (existence hidden, matches the
	// project's default-private posture).
	octoidentity.Set(nil)
	h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPost, "/v1/auth/login",
		map[string]string{"Content-Type": "application/json"},
		`{"token":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("login without provider = %d; want 404", rec.Code)
	}
}

func TestLoginEmptyTokenIs401(t *testing.T) {
	withStubIdentity(t, stubIdentity{uid: "u1", name: "Alice", role: "superAdmin"})
	h := newTestServer(t, loginCfg())
	rec := do(t, h, http.MethodPost, "/v1/auth/login",
		map[string]string{"Content-Type": "application/json"},
		`{}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty token = %d; want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "login_required") {
		t.Errorf("body missing login_required code: %s", rec.Body.String())
	}
}

func TestLoginInvalidTokenIs401(t *testing.T) {
	// Empty uid stub ⇒ VerifyToken returns nil ⇒ handler folds to 401.
	withStubIdentity(t, stubIdentity{uid: ""})
	h := newTestServer(t, loginCfg())
	rec := do(t, h, http.MethodPost, "/v1/auth/login",
		map[string]string{"Content-Type": "application/json"},
		`{"token":"nope"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token = %d; want 401", rec.Code)
	}
}

func TestLoginSuccessSetsCookieAndAuthMe(t *testing.T) {
	withStubIdentity(t, stubIdentity{uid: "u1", name: "Alice", role: "superAdmin"})
	h := newTestServer(t, loginCfg())

	rec := do(t, h, http.MethodPost, "/v1/auth/login",
		map[string]string{"Content-Type": "application/json"},
		`{"token":"tok"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
	}

	// The response body must NOT leak uid/token — cookie is the only credential.
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if data, _ := body["data"].(map[string]any); data == nil || data["ok"] != true {
		t.Fatalf("body = %v; want data.ok=true", body)
	}
	if strings.Contains(rec.Body.String(), "u1") {
		t.Error("uid leaked into login response body")
	}

	// Extract the odoc_sid cookie so we can replay it on /auth/me.
	var sid string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "odoc_sid" {
			sid = c.Value
			if !c.HttpOnly {
				t.Error("odoc_sid must be HttpOnly")
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Error("odoc_sid must be SameSite=Lax")
			}
			if c.MaxAge <= 0 {
				t.Errorf("odoc_sid MaxAge = %d; want > 0", c.MaxAge)
			}
		}
	}
	if sid == "" {
		t.Fatal("no odoc_sid cookie set on login success")
	}

	// /auth/me resolves the session and reports isOwner=true (superAdmin role
	// grants owner via Session.Role == "superAdmin") + authConfigured=true.
	rec = do(t, h, http.MethodGet, "/v1/auth/me",
		map[string]string{"Cookie": "odoc_sid=" + sid}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"isOwner":true`) {
		t.Errorf("superAdmin login did not grant isOwner: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"authConfigured":true`) {
		t.Errorf("authConfigured should follow OctoServerBaseURL: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"login":"u1"`) {
		t.Errorf("identity missing from me: %s", rec.Body.String())
	}
}

func TestAuthMeAuthConfiguredFollowsOctoServerBaseURL(t *testing.T) {
	// No login attempt — just prove /auth/me.authConfigured flips true when the
	// OCT-150 provider URL is configured, even with the legacy flag off.
	h := newTestServer(t, loginCfg())
	rec := do(t, h, http.MethodGet, "/v1/auth/me", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"authConfigured":true`) {
		t.Errorf("authConfigured should be true when OctoServerBaseURL set: %s", rec.Body.String())
	}
}
