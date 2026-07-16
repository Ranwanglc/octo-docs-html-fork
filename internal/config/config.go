// Package config holds the 12-factor application configuration. Every knob is an
// environment variable; the struct is parsed once at boot and treated as
// immutable thereafter. No other package reads the environment for app settings.
package config

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved, immutable application configuration.
type Config struct {
	Port    int
	Host    string
	BaseURL string
	RepoURL string

	// Storage: PostgreSQL/MySQL metadata + S3-compatible blobs.
	StorageDriver    string
	DatabaseURL      string
	PGPoolMax        int
	S3Bucket         string
	S3Region         string
	S3Endpoint       string
	S3ForcePathStyle bool
	S3AccessKeyID    string
	S3SecretKey      string

	WriteToken     string
	AllowBootstrap bool
	Owner          string
	// AssetSigningSecret keys the HMAC that signs asset sub-resource URLs so a
	// browser's native <img> load (which cannot carry the token header) is
	// authorized by a short-lived per-asset signature. Falls back to WriteToken
	// when unset so signing works without extra config.
	AssetSigningSecret string
	FrameAncestors     string
	// HostOrigins is the postMessage sender allowlist for the doc iframe (OCT-171).
	// Derived from FrameAncestors: same trust boundary as CSP frame-ancestors — hosts
	// allowed to iframe us are the hosts whose octo:init handshake we accept. CSP
	// source-list keywords like 'self'/'none'/* are dropped; only bare http(s) origins
	// survive. Empty ⇒ receiver silently rejects every incoming message.
	HostOrigins []string

	// LoginEnabled toggles the UI login affordance (/auth/me.authConfigured).
	// Under OCT-145 方案 C the reverse proxy is the login provider — flip this
	// on when octo-server's docs_proxy sits in front. Off ⇒ stand-alone deploy,
	// overlay renders anonymously. Does NOT gate the identity middleware: trust
	// headers are consumed unconditionally (the proxy is the only path in on
	// internal-network deploys), so a misconfigured flag cannot lock out the
	// admin.
	LoginEnabled bool
	// OctoServerBaseURL is octo-server's origin for the OCT-150 http-fallback
	// login provider (POST /v1/auth/verify). Set ⇒ /v1/auth/login is live and
	// LoginEnabled() reports true. Empty ⇒ provider off, only the reverse-proxy
	// trust-header path (OCT-145 方案 C) can log a viewer in.
	OctoServerBaseURL string
	// OctoServiceToken is an optional service token forwarded on GET /v1/users/:uid
	// when the login flow needs a follow-up user lookup. Empty ⇒ fall back to
	// the caller's own octo token. Never logged.
	OctoServiceToken string
	// BotAuthEnabled defaults off; enabling it also needs OCTO_SERVER_BASE_URL
	// so the process has an octoidentity provider to verify bot tokens.
	BotAuthEnabled bool
	// OctoDocBindingURL is octo-server's origin for the FEAT-3 doc_binding
	// channel (GET /v1/docs/bindings/:slug). Empty disables the channel; the
	// server falls back to trust-header identity + share-code semantics, so the
	// FEAT-3 rollout is a config flip.
	OctoDocBindingURL string
	// OctoDocBindingTTL bounds how long a doc_binding lookup is cached in
	// memory. Short by design (default 60s): a revoked binding must stop
	// granting cap quickly, but a burst of reads for the same slug must not
	// hammer octo-server.
	OctoDocBindingTTL time.Duration
	// OctoWebhookURL is the server-side comment-event webhook endpoint
	// (OCT-137/B). Empty ⇒ the doc-side webhook is disabled and comment
	// creation never triggers an IM push. Set to a full URL, e.g.
	// https://octo.example.com/v1/doc-webhook/comments.
	OctoWebhookURL string
	// OctoDocEventWebhookToken is the shared secret sent as
	// X-Octo-Doc-Webhook-Token. Server rejects with 503 (per contract) when
	// missing, so leaving this empty while OctoWebhookURL is set means every
	// notify fails — acceptable in dev, but production must configure both.
	OctoDocEventWebhookToken string
	// DocsBackendRegisterURL is docs-backend's POST /v1/bot/docs endpoint for
	// registering published HTML docs in the web-docs sidebar. Empty disables it.
	DocsBackendRegisterURL string
	// DocsBackendRegisterToken is the bot Bearer token used for registration
	// calls and the matching binding lookup. Never logged.
	DocsBackendRegisterToken string
	// TrustProxyHeaders enables honoring X-Forwarded-For / X-Real-IP for the client
	// IP (rate limiting). Enable ONLY when the server sits behind a trusted reverse
	// proxy that sets these; otherwise a client can spoof them to evade limits.
	TrustProxyHeaders bool
	// CORSOrigins is the allowlist of origins permitted on mutating /v1 routes. Empty
	// means no Access-Control-Allow-Origin is sent on writes (same-origin only).
	CORSOrigins []string

	RateLimitWindow time.Duration
	RateLimitMax    int
	MaxHTMLBytes    int64
	// MaxAssetBytes caps a single uploaded media asset. Assets are stored whole, so
	// this bounds per-request memory and object size. See docs/ASSETS.md.
	MaxAssetBytes int64
	// AssetMIMEAllow is the allowlist of MIME types accepted for asset uploads. The
	// server sniffs the bytes and rejects anything not in this set.
	AssetMIMEAllow []string
	LogLevel       string
	CookieSecure   bool

	IOTimeout time.Duration
	IORetries int
}

var slugRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// defaultAssetMIMEAllow is the conservative default set of MIME types accepted for
// media asset uploads: common images, audio/video, and PDF. See docs/ASSETS.md.
const defaultAssetMIMEAllow = "image/png,image/jpeg,image/gif,image/webp,image/avif,image/svg+xml," +
	"video/mp4,video/webm,audio/mpeg,audio/ogg,audio/wav,application/pdf"

// Load parses and validates configuration from the process environment.
func Load() (*Config, error) {
	c := &Config{
		Port:    envInt("PORT", 8080),
		Host:    env("HOST", "0.0.0.0"),
		BaseURL: strings.TrimRight(env("BASE_URL", ""), "/"),
		RepoURL: env("REPO_URL", "https://github.com/lml2468/octo-doc"),

		StorageDriver:    strings.ToLower(strings.TrimSpace(env("STORAGE_DRIVER", "postgres"))),
		DatabaseURL:      env("DATABASE_URL", env("PG_URL", "")),
		PGPoolMax:        envInt("PG_POOL_MAX", 10),
		S3Bucket:         env("S3_BUCKET", "octo-doc"),
		S3Region:         env("S3_REGION", env("AWS_REGION", "us-east-1")),
		S3Endpoint:       env("S3_ENDPOINT", ""),
		S3ForcePathStyle: envBool("S3_FORCE_PATH_STYLE", false),
		S3AccessKeyID:    env("S3_ACCESS_KEY_ID", env("AWS_ACCESS_KEY_ID", "")),
		S3SecretKey:      env("S3_SECRET_ACCESS_KEY", env("AWS_SECRET_ACCESS_KEY", "")),

		WriteToken:         env("WRITE_TOKEN", ""),
		AssetSigningSecret: env("ASSET_SIGNING_SECRET", ""),
		AllowBootstrap:     envBool("ALLOW_BOOTSTRAP", true),
		Owner:              strings.TrimSpace(env("OWNER", "")),
		FrameAncestors:     strings.TrimSpace(env("FRAME_ANCESTORS", "'none'")),

		LoginEnabled:      envBool("LOGIN_ENABLED", false),
		OctoServerBaseURL: strings.TrimSpace(env("OCTO_SERVER_BASE_URL", "")),
		OctoServiceToken:  env("OCTO_SERVICE_TOKEN", ""),
		BotAuthEnabled:    envBool("BOT_AUTH_ENABLED", false),
		// FEAT-3 doc_binding channel. Independent of the identity trust headers:
		// the probe only fires when an octo session is also present (see
		// capability.go), so leaving the URL set is inert on non-octo requests.
		OctoDocBindingURL: strings.TrimSpace(env("OCTO_DOC_BINDING_URL", "")),
		OctoDocBindingTTL: time.Duration(envInt("OCTO_DOC_BINDING_TTL_MS", 60_000)) * time.Millisecond,

		// OCT-137/B doc-side comment-event webhook. URL unset ⇒ webhook off;
		// token unset with URL set ⇒ server rejects (503) — logged, not fatal.
		OctoWebhookURL:           strings.TrimSpace(env("OCTO_WEBHOOK_URL", "")),
		OctoDocEventWebhookToken: env("OCTO_DOC_EVENT_WEBHOOK_TOKEN", ""),
		DocsBackendRegisterURL:   strings.TrimRight(strings.TrimSpace(env("DOCS_BACKEND_REGISTER_URL", "")), "/"),
		DocsBackendRegisterToken: env("DOCS_BACKEND_REGISTER_TOKEN", ""),

		TrustProxyHeaders: envBool("TRUST_PROXY_HEADERS", false),
		CORSOrigins:       splitList(env("CORS_ORIGINS", "")),

		RateLimitWindow: time.Duration(envInt("RATE_LIMIT_WINDOW_MS", 60_000)) * time.Millisecond,
		RateLimitMax:    envInt("RATE_LIMIT_MAX", 60),
		MaxHTMLBytes:    int64(envInt("MAX_HTML_BYTES", 5*1024*1024)),
		MaxAssetBytes:   int64(envInt("MAX_ASSET_BYTES", 25*1024*1024)),
		AssetMIMEAllow:  splitList(env("ASSET_MIME_ALLOW", defaultAssetMIMEAllow)),
		LogLevel:        env("LOG_LEVEL", "info"),
		CookieSecure:    envBool("COOKIE_SECURE", true),

		IOTimeout: time.Duration(envInt("IO_TIMEOUT_MS", 5000)) * time.Millisecond,
		IORetries: envInt("IO_RETRIES", 2),
	}
	c.HostOrigins = parseHostOrigins(c.FrameAncestors)
	return c, nil
}

// SafeSlug returns the slug if valid, or empty string. Single source of truth for
// slug validation.
func SafeSlug(slug string) string {
	if slugRe.MatchString(slug) {
		return slug
	}
	return ""
}

func env(key, dflt string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return dflt
}

// splitList parses a comma-separated env value into a trimmed, non-empty slice.
// An empty or all-whitespace input yields a nil slice (the loop skips empties).
func splitList(v string) []string {
	var out []string
	for part := range strings.SplitSeq(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseHostOrigins derives the postMessage receiver allowlist from a CSP
// frame-ancestors source-list (OCT-171). Only bare http(s) origins survive;
// CSP keywords ('self'/'none'/*/data:/blob:) and scheme+wildcard host tokens
// (e.g. https://*.example.com) are dropped — the receiver matches event.origin
// by exact string, and CSP host wildcards have no postMessage equivalent, so
// letting them through would silently widen the trust boundary while failing
// every real subdomain handshake. Trailing slashes are stripped so the value
// matches event.origin (which never carries one).
func parseHostOrigins(fa string) []string {
	var out []string
	for tok := range strings.FieldsSeq(fa) {
		t := strings.TrimRight(strings.TrimSpace(tok), "/")
		var hostPort string
		switch {
		case strings.HasPrefix(t, "https://"):
			hostPort = t[len("https://"):]
		case strings.HasPrefix(t, "http://"):
			hostPort = t[len("http://"):]
		default:
			continue
		}
		// Wildcard host has no postMessage equivalent — receiver would never fire.
		if strings.Contains(hostPort, "*") {
			continue
		}
		out = append(out, t)
	}
	return out
}

func envInt(key string, dflt int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return dflt
}

var truthyRe = regexp.MustCompile(`^(1|true|yes|on)$`)

func envBool(key string, dflt bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		return truthyRe.MatchString(strings.ToLower(strings.TrimSpace(v)))
	}
	return dflt
}

// Validate checks that required production settings are present. Returns a
// descriptive error listing every problem.
func (c *Config) Validate() error {
	var problems []string
	switch c.StorageDriver {
	case "", "postgres", "mysql":
	default:
		problems = append(problems, `STORAGE_DRIVER must be "postgres" or "mysql"`)
	}
	if c.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL is required")
	}
	if c.S3Bucket == "" {
		problems = append(problems, "S3_BUCKET is required")
	}
	if c.Port <= 0 || c.Port > 65535 {
		problems = append(problems, fmt.Sprintf("PORT must be 1..65535, got %d", c.Port))
	}
	if c.PGPoolMax <= 0 || c.PGPoolMax > math.MaxInt32 {
		problems = append(problems, fmt.Sprintf("PG_POOL_MAX must be 1..%d, got %d", math.MaxInt32, c.PGPoolMax))
	}
	if c.RateLimitMax < 0 {
		problems = append(problems, fmt.Sprintf("RATE_LIMIT_MAX must be >= 0, got %d", c.RateLimitMax))
	}
	if c.MaxHTMLBytes <= 0 {
		problems = append(problems, fmt.Sprintf("MAX_HTML_BYTES must be positive, got %d", c.MaxHTMLBytes))
	}
	if c.MaxAssetBytes <= 0 {
		problems = append(problems, fmt.Sprintf("MAX_ASSET_BYTES must be positive, got %d", c.MaxAssetBytes))
	}
	if len(c.AssetMIMEAllow) == 0 {
		problems = append(problems, "ASSET_MIME_ALLOW must list at least one MIME type")
	}
	// A custom S3 endpoint (MinIO/R2) has no ambient credential chain, so static
	// creds are required; on AWS the default chain may supply them, so only warn by
	// requiring them when an endpoint is set.
	if c.S3Endpoint != "" && (c.S3AccessKeyID == "" || c.S3SecretKey == "") {
		problems = append(problems, "S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY are required when S3_ENDPOINT is set")
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}
