// Package docsbackend registers published octo-doc HTML documents with
// docs-backend.
package docsbackend

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
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
const delegatedDeletePath = "/v1/internal/html-docs"

// CanonicalDocumentDeletedError marks a terminal idempotency-key replay whose
// canonical document was deleted. Callers must not retry or recreate locally.
type CanonicalDocumentDeletedError struct{}

func (*CanonicalDocumentDeletedError) Error() string { return "canonical document deleted" }

// IsCanonicalDocumentDeleted reports whether err is the stable terminal 410.
func IsCanonicalDocumentDeleted(err error) bool {
	_, ok := err.(*CanonicalDocumentDeletedError)
	return ok
}

// Registration is the POST /v1/bot/docs payload docs-backend accepts for
// octo-doc backed HTML documents.
type Registration struct {
	DocType        string `json:"docType"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	OctoDocSlug    string `json:"octoDocSlug,omitempty"`
	MountType      string `json:"mountType,omitempty"`
	Title          string `json:"title,omitempty"`
}

// RegistrationResult is the canonical docs-backend response for HTML doc
// registration. Created is false when the idempotent slug registration already
// existed.
type RegistrationResult struct {
	DocID       string `json:"docId"`
	OctoDocSlug string `json:"octoDocSlug"`

	ShareURL string `json:"shareUrl"`
	Created  bool   `json:"created"`
}

// Rename is the PATCH /v1/bot/docs/octo-doc/:slug payload.
type Rename struct {
	Title string `json:"title"`
}

// Published is the post-commit notification payload.
type Published struct {
	Title string `json:"title,omitempty"`
}

// DelegatedDelete is the exact human-delete payload accepted by docs-backend.
type DelegatedDelete struct {
	Slug       string `json:"slug"`
	DocID      string `json:"docId,omitempty"`
	ActorUID   string `json:"actorUid"`
	SuperAdmin bool   `json:"superAdmin"`
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

// Register POSTs an octo-doc registration and returns the canonical row.
func (c *Client) Register(ctx context.Context, reg Registration, token string) (*RegistrationResult, error) {
	if c == nil {
		return nil, fmt.Errorf("docs-backend registrar is disabled")
	}
	if strings.TrimSpace(token) == "" && strings.TrimSpace(reg.IdempotencyKey) != "" {
		return nil, fmt.Errorf("docs-backend canonical create requires publisher token")
	}
	body, err := c.doJSON(ctx, http.MethodPost, c.registerURL, reg, reg.OctoDocSlug, "register", token)
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
	if strings.TrimSpace(reg.IdempotencyKey) != "" && result.DocID != result.OctoDocSlug {
		return nil, fmt.Errorf("decode docs-backend registration: doc identity mismatch")
	}

	return &result, nil
}

// Rename PATCHes the registered title by octo-doc slug. token is the publishing
// bot's own bearer token; an empty token skips the optional mutation.
func (c *Client) Rename(ctx context.Context, slug, title, token string) {
	if c == nil || strings.TrimSpace(token) == "" {
		return
	}
	_, _ = c.doJSON(ctx, http.MethodPatch, c.octoDocURL(slug), Rename{Title: title}, slug, "rename", token)
}

// Delete removes the registered docs-backend row by octo-doc slug. It requires
// the request bot credential and propagates failures so callers can fail closed.
func (c *Client) Delete(ctx context.Context, slug, token string) error {
	if c == nil {
		return fmt.Errorf("docs-backend registrar is disabled")
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("docs-backend delete requires publisher token")
	}
	_, err := c.doJSONAllowNotFound(ctx, http.MethodDelete, c.octoDocURL(slug), nil, slug, "delete", token)
	return err
}

// DeleteDelegated signs exact JSON bytes and sends no authorization credential.
func (c *Client) DeleteDelegated(ctx context.Context, in DelegatedDelete, secret string) error {
	if c == nil {
		return fmt.Errorf("docs-backend registrar is disabled")
	}
	if strings.TrimSpace(secret) == "" {
		return fmt.Errorf("docs-backend delegated delete requires delegation secret")
	}
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal docs-backend delegated delete request: %w", err)
	}
	base, err := url.Parse(c.registerURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return fmt.Errorf("build docs-backend delegated delete request: invalid register URL")
	}
	endpoint := base.Scheme + "://" + base.Host + delegatedDeletePath
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	digest := sha256.Sum256(body)
	input := fmt.Sprintf("v1\nDELETE\n%s\n%s\n%x", delegatedDeletePath, timestamp, digest)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(input))
	signature := fmt.Sprintf("v1=%x", mac.Sum(nil))
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.http.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build docs-backend delegated delete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Octo-Timestamp", timestamp)
	req.Header.Set("X-Octo-Signature", signature)
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("docs-backend delegated delete request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return fmt.Errorf("read docs-backend delegated delete response: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("docs-backend delegated delete returned HTTP %d", resp.StatusCode)
}

// Published notifies docs-backend after HTML content and metadata are durable.
func (c *Client) Published(ctx context.Context, docID, title, token string) error {
	if c == nil {
		return fmt.Errorf("docs-backend registrar is disabled")
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("docs-backend publish notification requires publisher token")
	}
	_, err := c.doJSON(ctx, http.MethodPost, c.registerURL+"/"+url.PathEscape(docID)+"/published", Published{Title: title}, docID, "published", token)
	return err
}

func (c *Client) doJSONAllowNotFound(ctx context.Context, method, endpoint string, body any, slug, op, token string) ([]byte, error) {
	return c.doJSONStatus(ctx, method, endpoint, body, slug, op, token, true)
}

func (c *Client) octoDocURL(slug string) string {
	return c.registerURL + "/octo-doc/" + url.PathEscape(slug)
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, body any, slug, op, token string) ([]byte, error) {
	return c.doJSONStatus(ctx, method, endpoint, body, slug, op, token, false)
}

func (c *Client) doJSONStatus(ctx context.Context, method, endpoint string, body any, slug, op, token string, allowNotFound bool) ([]byte, error) {
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
	// Register/Rename retain the configured credential compatibility fallback.
	// Delete rejects an empty request token before reaching this helper.
	authToken := token
	if authToken == "" {
		authToken = c.token
	}
	if authToken != "" {
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
		if allowNotFound && resp.StatusCode == http.StatusNotFound {
			return respBody, nil
		}
		if op == "register" && resp.StatusCode == http.StatusGone {
			return nil, &CanonicalDocumentDeletedError{}
		}
		c.logger.Warn("docs_backend_register non-2xx", "slug", slug, "op", op, "http_status", resp.StatusCode)
		return nil, fmt.Errorf("docs-backend %s returned HTTP %d", op, resp.StatusCode)
	}
	return respBody, nil
}
