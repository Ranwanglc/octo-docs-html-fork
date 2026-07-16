package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8080 {
		t.Errorf("default port = %d, want 8080", cfg.Port)
	}
	if cfg.MaxHTMLBytes != 5*1024*1024 {
		t.Errorf("default max bytes = %d", cfg.MaxHTMLBytes)
	}
	if !cfg.AllowBootstrap {
		t.Error("bootstrap should default on")
	}
	if cfg.FrameAncestors != "'none'" {
		t.Errorf("frame ancestors = %q", cfg.FrameAncestors)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("PORT", "9999")
	t.Setenv("RATE_LIMIT_MAX", "0")
	t.Setenv("BASE_URL", "https://x.example.com/")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9999 {
		t.Errorf("port = %d", cfg.Port)
	}
	if cfg.RateLimitMax != 0 {
		t.Errorf("rate max = %d", cfg.RateLimitMax)
	}
	if cfg.BaseURL != "https://x.example.com" {
		t.Errorf("base url trailing slash not trimmed: %q", cfg.BaseURL)
	}
}

func TestValidate(t *testing.T) {
	// A minimally-valid config: required strings present + positive numeric knobs.
	valid := func() *Config {
		return &Config{
			DatabaseURL: "x", S3Bucket: "b",
			Port: 8080, PGPoolMax: 10, RateLimitMax: 60, MaxHTMLBytes: 1 << 20,
			MaxAssetBytes: 1 << 20, AssetMIMEAllow: []string{"image/png"},
		}
	}
	if err := valid().Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}

	c := valid()
	c.DatabaseURL = ""
	if err := c.Validate(); err == nil {
		t.Error("missing DATABASE_URL should fail validation")
	}
	c = valid()
	c.S3Bucket = ""
	if err := c.Validate(); err == nil {
		t.Error("missing S3_BUCKET should fail validation")
	}
	c = valid()
	c.Port = 0
	if err := c.Validate(); err == nil {
		t.Error("zero PORT should fail validation")
	}
	c = valid()
	c.MaxHTMLBytes = 0
	if err := c.Validate(); err == nil {
		t.Error("zero MAX_HTML_BYTES should fail validation")
	}
	c = valid()
	c.S3Endpoint = "http://minio:9000" // custom endpoint without creds
	if err := c.Validate(); err == nil {
		t.Error("S3_ENDPOINT without credentials should fail validation")
	}
}

func TestSafeSlug(t *testing.T) {
	valid := []string{"hello", "a_b-c", "ABC123", "x"}
	for _, s := range valid {
		if SafeSlug(s) != s {
			t.Errorf("SafeSlug(%q) rejected a valid slug", s)
		}
	}
	invalid := []string{"", "../etc", "a/b", "a b", "a.b", strings95()}
	for _, s := range invalid {
		if SafeSlug(s) != "" {
			t.Errorf("SafeSlug(%q) accepted an invalid slug", s)
		}
	}
}

func strings95() string {
	b := make([]byte, 95)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestLoadDocBindingKnobs(t *testing.T) {
	// FEAT-3: doc_binding URL + TTL come from env; unset means channel is off.
	t.Setenv("DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OctoDocBindingURL != "" {
		t.Errorf("unset OCTO_DOC_BINDING_URL should stay empty, got %q", cfg.OctoDocBindingURL)
	}
	if cfg.OctoDocBindingTTL.Milliseconds() != 60_000 {
		t.Errorf("default OCTO_DOC_BINDING_TTL = %s, want 60s", cfg.OctoDocBindingTTL)
	}
	t.Setenv("OCTO_DOC_BINDING_URL", "https://octo.example.com/")
	t.Setenv("OCTO_DOC_BINDING_TTL_MS", "5000")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OctoDocBindingURL != "https://octo.example.com/" {
		t.Errorf("OCTO_DOC_BINDING_URL = %q", cfg.OctoDocBindingURL)
	}
	if cfg.OctoDocBindingTTL.Milliseconds() != 5000 {
		t.Errorf("OCTO_DOC_BINDING_TTL_MS override = %s, want 5s", cfg.OctoDocBindingTTL)
	}
}

func TestLoadDocsBackendRegisterKnobs(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DocsBackendRegisterURL != "" || cfg.DocsBackendRegisterToken != "" {
		t.Fatalf("unset docs-backend register config = url %q token %q", cfg.DocsBackendRegisterURL, cfg.DocsBackendRegisterToken)
	}

	t.Setenv("DOCS_BACKEND_REGISTER_URL", "https://docs-backend.example.com/v1/bot/docs/")
	t.Setenv("DOCS_BACKEND_REGISTER_TOKEN", "bot-token")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DocsBackendRegisterURL != "https://docs-backend.example.com/v1/bot/docs" {
		t.Errorf("DOCS_BACKEND_REGISTER_URL = %q", cfg.DocsBackendRegisterURL)
	}
	if cfg.DocsBackendRegisterToken != "bot-token" {
		t.Errorf("DOCS_BACKEND_REGISTER_TOKEN = %q", cfg.DocsBackendRegisterToken)
	}
}

// TestParseHostOrigins covers the OCT-171 postMessage allowlist derivation:
// CSP keywords/wildcards must NOT bleed through (they have no postMessage
// equivalent), and trailing slashes must be stripped so the value matches
// event.origin (which never carries one).
func TestParseHostOrigins(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"none keyword", "'none'", nil},
		{"self keyword", "'self'", nil},
		{"wildcard", "*", nil},
		{"empty", "", nil},
		{"single https", "https://web.example.com", []string{"https://web.example.com"}},
		{"strip trailing slash", "https://web.example.com/", []string{"https://web.example.com"}},
		{"http scheme", "http://localhost:3000", []string{"http://localhost:3000"}},
		{"mixed with keyword", "'self' https://web.example.com", []string{"https://web.example.com"}},
		{"multiple origins", "https://a.example.com https://b.example.com", []string{"https://a.example.com", "https://b.example.com"}},
		{"reject bare host (no scheme)", "example.com", nil},
		{"reject data: scheme", "data:", nil},
		// host wildcards must be dropped: postMessage receiver does exact
		// event.origin match, so https://*.example.com would never fire on
		// real subdomains — silently breaking the handshake behind a passing CSP.
		{"reject scheme+wildcard host", "https://*.example.com", nil},
		{"mixed with wildcard host", "'self' https://ok.example.com https://*.bad.example.com", []string{"https://ok.example.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseHostOrigins(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestLoadHostOrigins verifies Load() wires FrameAncestors → HostOrigins so
// the doc-side receiver picks up the same trust boundary as CSP without a
// second env var.
func TestLoadHostOrigins(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("FRAME_ANCESTORS", "'self' https://web.example.com/")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.HostOrigins) != 1 || cfg.HostOrigins[0] != "https://web.example.com" {
		t.Fatalf("HostOrigins = %v, want [https://web.example.com]", cfg.HostOrigins)
	}
}
