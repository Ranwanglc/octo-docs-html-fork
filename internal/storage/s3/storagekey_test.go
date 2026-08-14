package s3

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// Regression guard for the storage_key migration: a legacy document has no
// Extra["storage_key"], so DocMeta.StorageKey() falls back to the slug and every
// object key must come out byte-identical to what the pre-storage_key code
// produced. The expectations below are HARD-CODED sha256 prefixes (computed
// out-of-band, not by calling storage.HashSlug), so a change to HashSlug or to
// the fallback would fail here instead of silently orphaning stored objects.
func TestLegacyMetaStorageKeyPreservesObjectKeys(t *testing.T) {
	// hex(sha256("hello-world"))[:32]
	const helloWorldHash = "afa27b44d43b02a9fea41d13cedc2e40"
	// hex(sha256("my-doc"))[:32]
	const myDocHash = "7134c69cbf15a3fef860d9a293f692f6"

	cases := []struct {
		name string
		meta *storage.DocMeta
		hash string
	}{
		{
			name: "legacy meta without storage_key falls back to slug",
			meta: &storage.DocMeta{Slug: "hello-world"},
			hash: helloWorldHash,
		},
		{
			name: "legacy meta with unrelated Extra keys still falls back to slug",
			meta: &storage.DocMeta{Slug: "hello-world", Extra: map[string]any{"title": "x"}},
			hash: helloWorldHash,
		},
		{
			name: "blank storage_key falls back to slug",
			meta: &storage.DocMeta{Slug: "hello-world", Extra: map[string]any{"storage_key": "   "}},
			hash: helloWorldHash,
		},
		{
			name: "explicit storage_key addresses that key, not the slug",
			meta: &storage.DocMeta{Slug: "hello-world", Extra: map[string]any{"storage_key": "my-doc"}},
			hash: myDocHash,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := tc.meta.StorageKey()
			s := &Store{root: ""}
			if got, want := s.prefixFor(key), "docs/"+tc.hash; got != want {
				t.Fatalf("prefixFor(%q) = %q, want %q", key, got, want)
			}
			if got, want := s.keyFor(key, 3), "docs/"+tc.hash+"/v3/index.html"; got != want {
				t.Fatalf("keyFor = %q, want %q", got, want)
			}
			if got, want := s.draftKeyFor(key), "docs/"+tc.hash+"/draft/index.html"; got != want {
				t.Fatalf("draftKeyFor = %q, want %q", got, want)
			}
			if got, want := s.assetKeyFor(key, "deadbeef"), "docs/"+tc.hash+"/assets/deadbeef"; got != want {
				t.Fatalf("assetKeyFor = %q, want %q", got, want)
			}
			// Same under a namespaced S3_PREFIX.
			ns := &Store{root: normalizeRoot("docs-html-prod")}
			if got, want := ns.keyFor(key, 1), "docs-html-prod/docs/"+tc.hash+"/v1/index.html"; got != want {
				t.Fatalf("namespaced keyFor = %q, want %q", got, want)
			}
		})
	}
}

// HashSlug itself is pinned: it is the one-way function every stored object key
// depends on, so its output must never drift.
func TestHashSlugPinnedValues(t *testing.T) {
	cases := map[string]string{
		"hello-world": "afa27b44d43b02a9fea41d13cedc2e40",
		"my-doc":      "7134c69cbf15a3fef860d9a293f692f6",
		"":            "e3b0c44298fc1c149afbf4c8996fb924",
	}
	for in, want := range cases {
		if got := storage.HashSlug(in); got != want {
			t.Errorf("HashSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
