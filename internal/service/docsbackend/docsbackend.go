// Package docsbackend registers published octo-doc HTML documents with
// docs-backend as a best-effort side channel.
package docsbackend

import (
	"bytes"
	"context"
	"encoding/json"
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
}

// Rename is the PATCH /v1/bot/docs/octo-doc/:slug payload.
type Rename struct {
	Title string `json:"title"`
}

// Client posts registration mutations. Empty URL returns nil from New; all
// methods are nil-safe and never return errors to callers.
type Client struct {
	registerURL string
	token       string
	http        *http.Client
	logger      *slog.Logger
}

// New wires the registrar. registerURL is the full POST endpoint, usually
// <docs-backend>/v1/bot/docs. token is sent as a bot Bearer token.
func New(registerURL, token string, logger *slog.Logger) *Client {
	return newWithTimeout(registerURL, token, defaultTimeout, logger)
}

func newWithTimeout(registerURL, token string, timeout time.Duration, logger *slog.Logger) *Client {
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
		registerURL: registerURL,
		token:       strings.TrimSpace(token),
		http:        &http.Client{Timeout: timeout},
		logger:      logger,
	}
}

// Register POSTs an octo-doc registration. token is the publishing bot's own
// bearer token: docs-backend reverse-resolves the doc's owner/space from it, so
// the doc is registered under whoever published it. Empty token falls back to
// the process-configured token (see doJSON).
func (c *Client) Register(ctx context.Context, reg Registration, token string) {
	if c == nil {
		return
	}
	c.doJSON(ctx, http.MethodPost, c.registerURL, reg, reg.OctoDocSlug, "register", token)
}

// Rename PATCHes the registered title by octo-doc slug. token is the publishing
// bot's own bearer token; empty falls back to the process-configured token.
func (c *Client) Rename(ctx context.Context, slug, title, token string) {
	if c == nil {
		return
	}
	c.doJSON(ctx, http.MethodPatch, c.octoDocURL(slug), Rename{Title: title}, slug, "rename", token)
}

// Delete removes the registered docs-backend row by octo-doc slug. Delete is
// by-slug and idempotent, so the caller identity is immaterial; token may be
// empty (falls back to the process-configured token).
func (c *Client) Delete(ctx context.Context, slug, token string) {
	if c == nil {
		return
	}
	c.doJSON(ctx, http.MethodDelete, c.octoDocURL(slug), nil, slug, "delete", token)
}

func (c *Client) octoDocURL(slug string) string {
	return c.registerURL + "/octo-doc/" + url.PathEscape(slug)
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, body any, slug, op, token string) {
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
			return
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		c.logger.Warn("docs_backend_register request build failed", "slug", slug, "op", op, "err", err.Error())
		return
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// Prefer the publishing bot's own token so docs-backend attributes the doc to
	// whoever published it; fall back to the process-configured token when the
	// caller had none (e.g. the by-slug delete path).
	authToken := token
	if authToken == "" {
		authToken = c.token
	}
	req.Header.Set("Authorization", "Bearer "+authToken)

	resp, err := c.http.Do(req)
	if err != nil {
		c.logger.Warn("docs_backend_register request failed", "slug", slug, "op", op, "err", err.Error())
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.logger.Warn("docs_backend_register non-2xx", "slug", slug, "op", op, "http_status", resp.StatusCode)
		return
	}
}
