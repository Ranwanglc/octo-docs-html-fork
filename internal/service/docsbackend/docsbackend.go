// Package docsbackend registers published octo-doc HTML documents with
// docs-backend.
package docsbackend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultTimeout = 5 * time.Second

// Registration is the POST /v1/bot/docs payload docs-backend accepts for
// octo-doc backed HTML documents.
type Registration struct {
	DocType     string `json:"docType"`
	OctoDocSlug string `json:"octoDocSlug"`
	MountType   string `json:"mountType"`
	Title       string `json:"title,omitempty"`
	Owner       string `json:"owner,omitempty"`
	SpaceID     string `json:"spaceId,omitempty"`
	Internal    bool   `json:"-"`
}

// RegistrationResult is the canonical docs-backend response for HTML doc
// registration. Created is false when the idempotent slug registration already
// existed.
type RegistrationResult struct {
	DocID       string `json:"docId"`
	OctoDocSlug string `json:"octoDocSlug"`
	ShareURL    string `json:"shareUrl"`
	Created     bool   `json:"created"`
}

// Rename is the PATCH /v1/bot/docs/octo-doc/:slug payload.
type Rename struct {
	Title string `json:"title"`
}

// Deletion identifies an HTML registration to remove.
type Deletion struct {
	OctoDocSlug string `json:"octoDocSlug"`
	SpaceID     string `json:"spaceId,omitempty"`
	Owner       string `json:"owner,omitempty"`
	UserPublish bool   `json:"-"`
}

// Client posts registration mutations. Empty URL returns nil from New; all
// methods are nil-safe and never return errors to callers.
type Client struct {
	registerURL   string
	botToken      string
	internalToken string
	http          *http.Client
	logger        *slog.Logger
}

// New wires the registrar. registerURL is the full POST endpoint, usually
// <docs-backend>/v1/bot/docs. token is sent as a bot Bearer token.
func New(registerURL, botToken, internalToken string, logger *slog.Logger) *Client {
	return newWithTimeout(registerURL, botToken, internalToken, defaultTimeout, logger)
}

func newWithTimeout(registerURL, botToken, internalToken string, timeout time.Duration, logger *slog.Logger) *Client {
	registerURL = strings.TrimRight(strings.TrimSpace(registerURL), "/")
	if registerURL == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		registerURL:   registerURL,
		botToken:      strings.TrimSpace(botToken),
		internalToken: strings.TrimSpace(internalToken),
		http: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		logger: logger,
	}
}

// Register POSTs an octo-doc registration and returns the canonical row.
func (c *Client) Register(ctx context.Context, reg Registration, token string) (*RegistrationResult, error) {
	if c == nil {
		return nil, fmt.Errorf("docs-backend registrar is disabled")
	}
	endpoint := c.registerURL
	if reg.Internal {
		endpoint = internalRegisterURL(endpoint)
		if c.internalToken == "" {
			return nil, fmt.Errorf("docs-backend internal register token is not configured")
		}
	}
	body, err := c.doJSON(ctx, http.MethodPost, endpoint, reg, reg.OctoDocSlug, "register", token, reg.Internal)
	if err != nil {
		return nil, err
	}
	var result RegistrationResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode docs-backend registration: %w", err)
	}
	if strings.TrimSpace(result.DocID) == "" || strings.TrimSpace(result.OctoDocSlug) == "" || strings.TrimSpace(result.ShareURL) == "" {
		return nil, fmt.Errorf("decode docs-backend registration: required response field missing")
	}
	if result.OctoDocSlug != reg.OctoDocSlug {
		return nil, fmt.Errorf("decode docs-backend registration: slug mismatch %q", result.OctoDocSlug)
	}
	return &result, nil
}

func internalRegisterURL(registerURL string) string {
	if strings.HasSuffix(registerURL, "/v1/bot/docs") {
		return strings.TrimSuffix(registerURL, "/v1/bot/docs") + "/internal/html/register"
	}
	return strings.TrimRight(registerURL, "/") + "/internal/html/register"
}

func internalDeleteURL(registerURL string) string {
	if strings.HasSuffix(registerURL, "/v1/bot/docs") {
		return strings.TrimSuffix(registerURL, "/v1/bot/docs") + "/internal/html"
	}
	return strings.TrimRight(registerURL, "/") + "/internal/html"
}

// Rename PATCHes the registered title by octo-doc slug. token is the publishing
// bot's own bearer token; empty falls back to the process-configured token.
func (c *Client) Rename(ctx context.Context, slug, title, token string) {
	if c == nil {
		return
	}
	_, _ = c.doJSON(ctx, http.MethodPatch, c.octoDocURL(slug), Rename{Title: title}, slug, "rename", token, false)
}

// Delete removes the registered docs-backend row by octo-doc slug. Delete is
// by-slug and idempotent, so the caller identity is immaterial; token may be
// empty (falls back to the process-configured token).
func (c *Client) Delete(ctx context.Context, deletion Deletion, token string) error {
	if c == nil {
		return fmt.Errorf("docs-backend registrar is disabled")
	}
	if deletion.UserPublish {
		if c.internalToken == "" {
			return fmt.Errorf("docs-backend internal register token is not configured")
		}
		_, err := c.doJSON(ctx, http.MethodDelete, internalDeleteURL(c.registerURL), deletion, deletion.OctoDocSlug, "delete", "", true)
		return err
	}
	_, err := c.doJSON(ctx, http.MethodDelete, c.octoDocURL(deletion.OctoDocSlug), nil, deletion.OctoDocSlug, "delete", token, false)
	return err
}

func (c *Client) octoDocURL(slug string) string {
	return c.registerURL + "/octo-doc/" + url.PathEscape(slug)
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, body any, slug, op, token string, internal bool) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.http.Timeout)
	defer cancel()

	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			c.logger.Warn("docs_backend_register marshal failed", "slug", slug, "op", op, "err", err.Error())
			return nil, fmt.Errorf("marshal docs-backend %s request: %w", op, err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		c.logger.Warn("docs_backend_register request build failed", "slug", slug, "op", op, "err", err.Error())
		return nil, fmt.Errorf("build docs-backend %s request: %w", op, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// Prefer the publishing bot's own token so docs-backend attributes the doc to
	// whoever published it; fall back to the process-configured token when the
	// caller had none (e.g. the by-slug delete path).
	if internal {
		req.Header.Set("X-Internal-Token", c.internalToken)
	} else {
		authToken := token
		if authToken == "" {
			authToken = c.botToken
		}
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.logger.Warn("docs_backend_register request failed", "slug", slug, "op", op, "err", err.Error())
		return nil, fmt.Errorf("docs-backend %s request: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, fmt.Errorf("read docs-backend %s response: %w", op, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.logger.Warn("docs_backend_register non-2xx", "slug", slug, "op", op, "http_status", resp.StatusCode)
		return nil, fmt.Errorf("docs-backend %s returned HTTP %d", op, resp.StatusCode)
	}
	return respBody, nil
}
