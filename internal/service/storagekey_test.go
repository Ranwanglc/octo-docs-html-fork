package service

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

// storageKeyOf must never move a document's blob addressing: absent metadata and
// legacy metadata (no storage_key) both resolve to the slug, which is exactly the
// key those objects were written under.
func TestStorageKeyOfFallsBackToSlug(t *testing.T) {
	cases := []struct {
		name string
		meta *storage.DocMeta
		slug string
		want string
	}{
		{"nil meta", nil, "legacy-doc", "legacy-doc"},
		{"nil Extra", &storage.DocMeta{Slug: "legacy-doc"}, "legacy-doc", "legacy-doc"},
		{"empty Extra", &storage.DocMeta{Slug: "legacy-doc", Extra: map[string]any{}}, "legacy-doc", "legacy-doc"},
		{
			"unrelated Extra keys",
			&storage.DocMeta{Slug: "legacy-doc", Extra: map[string]any{storage.CreatorUIDExtraKey: "u1"}},
			"legacy-doc", "legacy-doc",
		},
		{
			"blank storage_key",
			&storage.DocMeta{Slug: "legacy-doc", Extra: map[string]any{storage.StorageKeyExtraKey: " "}},
			"legacy-doc", "legacy-doc",
		},
		{
			"non-string storage_key",
			&storage.DocMeta{Slug: "legacy-doc", Extra: map[string]any{storage.StorageKeyExtraKey: 42}},
			"legacy-doc", "legacy-doc",
		},
		{
			"explicit storage_key wins",
			&storage.DocMeta{Slug: "public-name", Extra: map[string]any{storage.StorageKeyExtraKey: "sk-123"}},
			"public-name", "sk-123",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := storageKeyOf(tc.meta, tc.slug); got != tc.want {
				t.Fatalf("storageKeyOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveStorageKey(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })

	// Unknown slug ⇒ slug itself (nothing stored yet).
	got, err := resolveStorageKey(ctx, store, "brand-new")
	if err != nil {
		t.Fatalf("resolveStorageKey: %v", err)
	}
	if got != "brand-new" {
		t.Fatalf("resolveStorageKey(new) = %q, want %q", got, "brand-new")
	}

	// Legacy meta with no storage_key ⇒ slug.
	if err := store.PutMeta(ctx, "legacy", storage.DocMeta{Slug: "legacy"}); err != nil {
		t.Fatalf("PutMeta: %v", err)
	}
	got, err = resolveStorageKey(ctx, store, "legacy")
	if err != nil {
		t.Fatalf("resolveStorageKey: %v", err)
	}
	if got != "legacy" {
		t.Fatalf("resolveStorageKey(legacy) = %q, want %q", got, "legacy")
	}

	// Persisted storage_key ⇒ that key.
	if err := store.PutMeta(ctx, "renamed", storage.DocMeta{
		Slug:  "renamed",
		Extra: map[string]any{storage.StorageKeyExtraKey: "sk-abc"},
	}); err != nil {
		t.Fatalf("PutMeta: %v", err)
	}
	got, err = resolveStorageKey(ctx, store, "renamed")
	if err != nil {
		t.Fatalf("resolveStorageKey: %v", err)
	}
	if got != "sk-abc" {
		t.Fatalf("resolveStorageKey(renamed) = %q, want %q", got, "sk-abc")
	}
}
