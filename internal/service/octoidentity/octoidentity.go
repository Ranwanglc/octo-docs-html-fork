// Package octoidentity is doc's client to octo-server's identity endpoints.
//
// Two capabilities (mirror of octo-docs-backend/src/auth/octoIdentity.ts, §4.7):
//
//	(a) token     -> trusted uid : POST /v1/auth/verify
//	(b) bot token -> trusted bot : POST /v1/auth/verify-bot
//	(c) uid       -> user info   : GET  /v1/users/:uid
//
// Only used on the http-fallback path (OCT-150): a browser that hits doc
// directly (no docs_proxy trust headers) exchanges its octo token for an
// odoc_sid cookie. In the reverse-proxy path (OCT-145 方案 C) doc trusts the
// X-Octo-* headers and never talks to this client.
package octoidentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// User is the identity fields doc consumes. Role feeds AuthService.IsOwner
// (via Session.Role == "superAdmin"); other fields land in the session.
type User struct {
	UID    string
	Name   string
	Avatar string
	Role   string
}

// BotIdentity is the identity fields doc needs to turn a verified bot token
// into an in-memory author session.
type BotIdentity struct {
	UID      string
	Name     string
	SpaceID  string
	OwnerUID string
}

// Identity is the injectable seam: HTTP impl in prod, stub in tests.
type Identity interface {
	// VerifyToken resolves an octo token to a trusted user. Returns (nil, nil)
	// when the token is missing/invalid or the upstream is unreachable — the
	// caller decides how to gate (typically 401 login_required). A non-nil
	// error is reserved for programmer bugs, not for auth failures.
	VerifyToken(ctx context.Context, token string) (*User, error)
	// VerifyBot resolves a bot token to a trusted bot. Same nil-nil contract as
	// VerifyToken; callers use nil as "not a bot token" and continue fallback
	// auth chains.
	VerifyBot(ctx context.Context, botToken string) (*BotIdentity, error)
	// GetUser looks up a user by uid. callerToken is used as the `token`
	// header when no service token is configured. Same nil-nil contract as
	// VerifyToken on 404/unreachable/malformed.
	GetUser(ctx context.Context, uid, callerToken string) (*User, error)
}

// HTTPIdentity talks to octo-server over HTTP. Zero-value is not usable — go
// through New.
type HTTPIdentity struct {
	baseURL      string
	serviceToken string
	client       *http.Client
}

// New builds an HTTPIdentity. baseURL must not have a trailing slash; timeout
// bounds every request (defaults to 5s when non-positive).
func New(baseURL, serviceToken string, timeout time.Duration) *HTTPIdentity {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPIdentity{
		baseURL:      strings.TrimRight(baseURL, "/"),
		serviceToken: serviceToken,
		client:       &http.Client{Timeout: timeout},
	}
}

// verifyBody mirrors octo-server modules/user/api.go authVerifyToken response.
// role and owned_bots are surfaced by the server; doc only cares about role.
type verifyBody struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// VerifyToken → POST /v1/auth/verify {token}. Unauthenticated / network
// failure / malformed body all fold to (nil, nil) so the http handler can map
// them to a single 401 without a taxonomy of upstream errors.
func (h *HTTPIdentity) VerifyToken(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, nil
	}
	payload, _ := json.Marshal(map[string]string{"token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/v1/auth/verify", strings.NewReader(string(payload)))
	if err != nil {
		return nil, nil
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := h.client.Do(req)
	if err != nil {
		slog.Default().Warn("octoidentity: upstream fault", "op", "verifyToken", "err", err.Error())
		return nil, nil
	}
	defer func() { _, _ = io.Copy(io.Discard, res.Body); _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if res.StatusCode >= 500 && res.StatusCode < 600 {
			slog.Default().Warn("octoidentity: upstream fault", "op", "verifyToken", "status", res.StatusCode)
		}
		return nil, nil
	}
	var body verifyBody
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		slog.Default().Warn("octoidentity: upstream fault", "op", "verifyToken", "err", err.Error())
		return nil, nil
	}
	if body.UID == "" {
		return nil, nil
	}
	return &User{UID: body.UID, Name: body.Name, Role: body.Role}, nil
}

type verifyBotBody struct {
	BotUID   string `json:"bot_uid"`
	BotName  string `json:"bot_name"`
	SpaceID  string `json:"space_id"`
	OwnerUID string `json:"owner_uid"`
}

// VerifyBot → POST /v1/auth/verify-bot {bot_token}. Bot grants are scoped by
// octo-server; doc rejects unscoped bot identities so a malformed upstream
// response cannot create a global author session.
func (h *HTTPIdentity) VerifyBot(ctx context.Context, botToken string) (*BotIdentity, error) {
	if botToken == "" {
		return nil, nil
	}
	payload, _ := json.Marshal(map[string]string{"bot_token": botToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/v1/auth/verify-bot", strings.NewReader(string(payload)))
	if err != nil {
		return nil, nil
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := h.client.Do(req)
	if err != nil {
		slog.Default().Warn("octoidentity: upstream fault", "op", "verifyBot", "err", err.Error())
		return nil, nil
	}
	defer func() { _, _ = io.Copy(io.Discard, res.Body); _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if res.StatusCode >= 500 && res.StatusCode < 600 {
			slog.Default().Warn("octoidentity: upstream fault", "op", "verifyBot", "status", res.StatusCode)
		}
		return nil, nil
	}
	var body verifyBotBody
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		slog.Default().Warn("octoidentity: upstream fault", "op", "verifyBot", "err", err.Error())
		return nil, nil
	}
	botUID := strings.TrimSpace(body.BotUID)
	spaceID := strings.TrimSpace(body.SpaceID)
	if botUID == "" || spaceID == "" {
		return nil, nil
	}
	return &BotIdentity{
		UID:      botUID,
		Name:     strings.TrimSpace(body.BotName),
		SpaceID:  spaceID,
		OwnerUID: strings.TrimSpace(body.OwnerUID),
	}, nil
}

type userBody struct {
	UID            string `json:"uid"`
	Name           string `json:"name"`
	Role           string `json:"role"`
	IsUploadAvatar int    `json:"is_upload_avatar"`
	AvatarVersion  int64  `json:"avatar_version"`
}

// GetUser → GET /v1/users/{uid}. octo-server requires auth on that route, so
// we send `token` header: service token if configured, else the caller's own.
// Same nil-nil contract on 404 / non-2xx / malformed / network failure.
func (h *HTTPIdentity) GetUser(ctx context.Context, uid, callerToken string) (*User, error) {
	if uid == "" {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/v1/users/"+uid, nil)
	if err != nil {
		return nil, nil
	}
	if tok := h.serviceToken; tok != "" {
		req.Header.Set("token", tok)
	} else if callerToken != "" {
		req.Header.Set("token", callerToken)
	}
	res, err := h.client.Do(req)
	if err != nil {
		slog.Default().Warn("octoidentity: upstream fault", "op", "getUser", "err", err.Error())
		return nil, nil
	}
	defer func() { _, _ = io.Copy(io.Discard, res.Body); _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if res.StatusCode >= 500 && res.StatusCode < 600 {
			slog.Default().Warn("octoidentity: upstream fault", "op", "getUser", "status", res.StatusCode)
		}
		return nil, nil
	}
	var body userBody
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		slog.Default().Warn("octoidentity: upstream fault", "op", "getUser", "err", err.Error())
		return nil, nil
	}
	if body.UID == "" {
		return nil, nil
	}
	var avatar string
	if body.IsUploadAvatar == 1 {
		avatar = fmt.Sprintf("%s/v1/users/%s/avatar?v=%d", h.baseURL, body.UID, body.AvatarVersion)
	}
	return &User{UID: body.UID, Name: body.Name, Avatar: avatar, Role: body.Role}, nil
}

// ErrDisabled is returned by Get when no provider has been configured. Callers
// use this to distinguish "provider off" (skip / 404) from "verify failed"
// (401). It is not a runtime failure.
var ErrDisabled = errors.New("octoidentity: provider disabled")

var (
	mu       sync.RWMutex
	provider Identity
)

// Set installs the process-wide identity provider. Pass nil to clear (tests
// use this to reset between cases). Wiring happens once at boot from config.
func Set(impl Identity) {
	mu.Lock()
	provider = impl
	mu.Unlock()
}

// Get returns the installed provider, or ErrDisabled when none is set. Http
// handlers use ErrDisabled to short-circuit before touching request state.
func Get() (Identity, error) {
	mu.RLock()
	p := provider
	mu.RUnlock()
	if p == nil {
		return nil, ErrDisabled
	}
	return p, nil
}
