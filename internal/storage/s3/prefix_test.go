package s3

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// White-box test: S3_PREFIX namespaces every object key so one bucket can
// isolate environments. Empty prefix preserves the legacy bare docs/ layout.
func TestKeyPrefixNamespacing(t *testing.T) {
	const slug = "hello-world"
	hashed := "docs/" + storage.HashSlug(slug)

	cases := []struct {
		name   string
		prefix string
		root   string // expected namespace root
	}{
		{"empty keeps legacy layout", "", ""},
		{"test env", "docs-html-test", "docs-html-test/"},
		{"prod env", "docs-html-prod", "docs-html-prod/"},
		{"surrounding slashes trimmed", "/docs-html-test/", "docs-html-test/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRoot(tc.prefix); got != tc.root {
				t.Fatalf("normalizeRoot(%q): got %q want %q", tc.prefix, got, tc.root)
			}
			s := &Store{root: normalizeRoot(tc.prefix)}
			if got, want := s.keyFor(slug, 1), tc.root+hashed+"/v1/index.html"; got != want {
				t.Fatalf("keyFor: got %q want %q", got, want)
			}
			if got, want := s.draftKeyFor(slug), tc.root+hashed+"/draft/index.html"; got != want {
				t.Fatalf("draftKeyFor: got %q want %q", got, want)
			}
			if got, want := s.assetKeyFor(slug, "abc"), tc.root+hashed+"/assets/abc"; got != want {
				t.Fatalf("assetKeyFor: got %q want %q", got, want)
			}
		})
	}
}
