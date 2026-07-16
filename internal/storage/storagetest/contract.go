// Package storagetest provides a reusable contract suite that any MetadataStore
// and BlobStore implementation must satisfy. It is run against the in-memory
// store here and against PostgreSQL+S3 in the e2e suite, proving the adapters are
// interchangeable.
package storagetest

import (
	"bytes"
	"context"
	"testing"

	"github.com/lml2468/octo-doc/internal/core"
	"github.com/lml2468/octo-doc/internal/storage"
)

// RunMetadata exercises the MetadataStore contract. The store must be empty.
func RunMetadata(t *testing.T, ms storage.MetadataStore) {
	t.Helper()
	ctx := context.Background()

	t.Run("meta crud", func(t *testing.T) {
		if m, err := ms.GetMeta(ctx, "absent"); err != nil || m != nil {
			t.Fatalf("GetMeta absent = %v, %v; want nil, nil", m, err)
		}
		meta := storage.DocMeta{Slug: "doc1", Title: "Doc One", Versions: []storage.VersionRef{{N: 1}}}
		if err := ms.PutMeta(ctx, "doc1", meta); err != nil {
			t.Fatal(err)
		}
		got, err := ms.GetMeta(ctx, "doc1")
		if err != nil || got == nil {
			t.Fatalf("GetMeta = %v, %v", got, err)
		}
		if got.Title != "Doc One" || len(got.Versions) != 1 || got.Versions[0].N != 1 {
			t.Fatalf("meta roundtrip mismatch: %+v", got)
		}
		list, err := ms.ListMeta(ctx)
		if err != nil || len(list) != 1 || list[0].Slug != "doc1" {
			t.Fatalf("ListMeta = %v, %v", list, err)
		}
		if err := ms.DeleteMeta(ctx, "doc1"); err != nil {
			t.Fatal(err)
		}
		if m, _ := ms.GetMeta(ctx, "doc1"); m != nil {
			t.Fatal("meta not deleted")
		}
	})

	t.Run("meta extra fields preserved", func(t *testing.T) {
		meta := storage.DocMeta{Slug: "doc2", Title: "T", Versions: []storage.VersionRef{},
			Extra: map[string]any{"custom": "value"}}
		if err := ms.PutMeta(ctx, "doc2", meta); err != nil {
			t.Fatal(err)
		}
		got, _ := ms.GetMeta(ctx, "doc2")
		if got == nil || got.Extra["custom"] != "value" {
			t.Fatalf("extra not preserved: %+v", got)
		}
		_ = ms.DeleteMeta(ctx, "doc2")
	})

	t.Run("comments crud", func(t *testing.T) {
		if list, err := ms.GetComments(ctx, "absent"); err != nil || len(list) != 0 {
			t.Fatalf("GetComments absent = %v, %v; want empty", list, err)
		}
		comments := []core.Comment{{
			ID: "c1", Author: &core.Author{Login: "a"}, Created: "t", CreatedIn: 1,
			Events: []core.CommentEvent{{Kind: "created", EID: "e1", AtVersion: 1, At: "t", Text: "hi"}},
		}}
		if err := ms.PutComments(ctx, "doc1", comments); err != nil {
			t.Fatal(err)
		}
		got, err := ms.GetComments(ctx, "doc1")
		if err != nil || len(got) != 1 || got[0].ID != "c1" {
			t.Fatalf("GetComments = %v, %v", got, err)
		}
		if err := ms.DeleteComments(ctx, "doc1"); err != nil {
			t.Fatal(err)
		}
		if list, _ := ms.GetComments(ctx, "doc1"); len(list) != 0 {
			t.Fatal("comments not deleted")
		}
	})

	t.Run("sessions with ttl", func(t *testing.T) {
		if s, err := ms.GetSession(ctx, "absent"); err != nil || s != nil {
			t.Fatalf("GetSession absent = %v, %v", s, err)
		}
		sess := storage.Session{Login: "bob", Created: "t"}
		if err := ms.PutSession(ctx, "sid1", sess, 3600); err != nil {
			t.Fatal(err)
		}
		got, err := ms.GetSession(ctx, "sid1")
		if err != nil || got == nil || got.Login != "bob" {
			t.Fatalf("GetSession = %v, %v", got, err)
		}
		// expired session must not resolve
		if err := ms.PutSession(ctx, "sid2", sess, -1); err != nil {
			t.Fatal(err)
		}
		if got, _ := ms.GetSession(ctx, "sid2"); got != nil {
			t.Fatal("expired session resolved")
		}
		if err := ms.DeleteSession(ctx, "sid1"); err != nil {
			t.Fatal(err)
		}
		if got, _ := ms.GetSession(ctx, "sid1"); got != nil {
			t.Fatal("session not deleted")
		}
	})

	t.Run("tokens", func(t *testing.T) {
		if ok, err := ms.AnyToken(ctx); err != nil || ok {
			t.Fatalf("AnyToken empty = %v, %v; want false", ok, err)
		}
		rec := storage.TokenRecord{Token: "tok1", Created: "t", Label: "bootstrap"}
		if err := ms.PutToken(ctx, "tok1", rec); err != nil {
			t.Fatal(err)
		}
		got, err := ms.GetToken(ctx, "tok1")
		if err != nil || got == nil || got.Label != "bootstrap" {
			t.Fatalf("GetToken = %v, %v", got, err)
		}
		// ON CONFLICT DO NOTHING: second put with same key keeps the first.
		_ = ms.PutToken(ctx, "tok1", storage.TokenRecord{Token: "tok1", Label: "other"})
		got2, _ := ms.GetToken(ctx, "tok1")
		if got2.Label != "bootstrap" {
			t.Fatalf("token overwritten: %+v", got2)
		}
		if ok, _ := ms.AnyToken(ctx); !ok {
			t.Fatal("AnyToken should be true")
		}
	})

	t.Run("assets", func(t *testing.T) {
		if a, err := ms.GetAssetMeta(ctx, "d", "absent"); err != nil || a != nil {
			t.Fatalf("GetAssetMeta absent = %v, %v; want nil, nil", a, err)
		}
		am := storage.AssetMeta{Slug: "d", SHA256: "abc", MIME: "image/png", Size: 12, OriginalName: "a.png", Created: "t"}
		if err := ms.PutAssetMeta(ctx, am); err != nil {
			t.Fatal(err)
		}
		got, err := ms.GetAssetMeta(ctx, "d", "abc")
		if err != nil || got == nil || got.MIME != "image/png" || got.Size != 12 {
			t.Fatalf("GetAssetMeta = %+v, %v", got, err)
		}
		// Re-put with same key is idempotent (ON CONFLICT DO UPDATE).
		if err := ms.PutAssetMeta(ctx, am); err != nil {
			t.Fatal(err)
		}
		// A second asset under the same slug + one under another slug.
		_ = ms.PutAssetMeta(ctx, storage.AssetMeta{Slug: "d", SHA256: "def", MIME: "image/gif", Size: 3, OriginalName: "b.gif", Created: "t"})
		_ = ms.PutAssetMeta(ctx, storage.AssetMeta{Slug: "other", SHA256: "xyz", MIME: "image/png", Size: 1, OriginalName: "c.png", Created: "t"})
		list, err := ms.ListAssetMeta(ctx, "d")
		if err != nil || len(list) != 2 {
			t.Fatalf("ListAssetMeta = %v (%d), %v; want 2 for slug d", list, len(list), err)
		}
		if err := ms.DeleteAssetMeta(ctx, "d", "abc"); err != nil {
			t.Fatal(err)
		}
		if a, _ := ms.GetAssetMeta(ctx, "d", "abc"); a != nil {
			t.Fatal("asset meta not deleted")
		}
		if l, _ := ms.ListAssetMeta(ctx, "d"); len(l) != 1 {
			t.Fatalf("ListAssetMeta after delete = %d; want 1", len(l))
		}
		// ListAssetSlugs returns every slug with assets, deduped and sorted.
		slugs, err := ms.ListAssetSlugs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(slugs) != 2 || slugs[0] != "d" || slugs[1] != "other" {
			t.Fatalf("ListAssetSlugs = %v; want [d other]", slugs)
		}
	})
}

// RunBlob exercises the BlobStore contract. The store must be empty.
func RunBlob(t *testing.T, bs storage.BlobStore) {
	t.Helper()
	ctx := context.Background()

	if _, ok, err := bs.GetDoc(ctx, "absent", 1); err != nil || ok {
		t.Fatalf("GetDoc absent = ok %v, err %v; want false", ok, err)
	}
	if _, ok, err := bs.HeadDoc(ctx, "absent", 1); err != nil || ok {
		t.Fatalf("HeadDoc absent = ok %v, err %v; want false", ok, err)
	}

	html := "<html><body>hello</body></html>"
	size, err := bs.PutDoc(ctx, "blogslug", 1, html)
	if err != nil || size != int64(len(html)) {
		t.Fatalf("PutDoc = %d, %v", size, err)
	}
	got, ok, err := bs.GetDoc(ctx, "blogslug", 1)
	if err != nil || !ok || got != html {
		t.Fatalf("GetDoc = %q, %v, %v", got, ok, err)
	}
	hsize, hok, err := bs.HeadDoc(ctx, "blogslug", 1)
	if err != nil || !hok || hsize != int64(len(html)) {
		t.Fatalf("HeadDoc = %d, %v, %v", hsize, hok, err)
	}

	if _, err := bs.PutDoc(ctx, "blogslug", 2, "v2"); err != nil {
		t.Fatal(err)
	}
	versions, err := bs.ListVersions(ctx, "blogslug")
	if err != nil || len(versions) != 2 || versions[0] != 1 || versions[1] != 2 {
		t.Fatalf("ListVersions = %v, %v; want [1 2]", versions, err)
	}

	if err := bs.DeleteDoc(ctx, "blogslug"); err != nil {
		t.Fatal(err)
	}
	if v, _ := bs.ListVersions(ctx, "blogslug"); len(v) != 0 {
		t.Fatalf("versions after delete = %v; want empty", v)
	}

	// Draft slot: mutable, overwritable, and invisible to ListVersions.
	if _, ok, err := bs.GetDraft(ctx, "draftslug"); err != nil || ok {
		t.Fatalf("GetDraft absent = ok %v, err %v; want false", ok, err)
	}
	if _, err := bs.PutDraft(ctx, "draftslug", "<html>draft v1</html>"); err != nil {
		t.Fatal(err)
	}
	if _, err := bs.PutDraft(ctx, "draftslug", "<html>draft v2</html>"); err != nil {
		t.Fatal(err) // overwrite must succeed
	}
	d, ok, err := bs.GetDraft(ctx, "draftslug")
	if err != nil || !ok || d != "<html>draft v2</html>" {
		t.Fatalf("GetDraft = %q, %v, %v; want the overwritten value", d, ok, err)
	}
	// A draft must never register as a version.
	if v, _ := bs.ListVersions(ctx, "draftslug"); len(v) != 0 {
		t.Fatalf("draft leaked into ListVersions: %v", v)
	}
	if err := bs.DeleteDraft(ctx, "draftslug"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := bs.GetDraft(ctx, "draftslug"); ok {
		t.Fatal("draft still present after DeleteDraft")
	}
	// DeleteDraft on an absent slug is not an error.
	if err := bs.DeleteDraft(ctx, "neverexisted"); err != nil {
		t.Fatalf("DeleteDraft absent = %v; want nil", err)
	}

	// Assets: content-addressed bytes under the doc's prefix.
	if _, ok, err := bs.GetAsset(ctx, "assetslug", "deadbeef"); err != nil || ok {
		t.Fatalf("GetAsset absent = ok %v, err %v; want false", ok, err)
	}
	raw := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	if err := bs.PutAsset(ctx, "assetslug", "deadbeef", raw); err != nil {
		t.Fatal(err)
	}
	if err := bs.PutAsset(ctx, "assetslug", "deadbeef", raw); err != nil {
		t.Fatal(err) // idempotent re-put must succeed
	}
	ab, ok, err := bs.GetAsset(ctx, "assetslug", "deadbeef")
	if err != nil || !ok || !bytes.Equal(ab, raw) {
		t.Fatalf("GetAsset = %v, %v, %v; want the bytes", ab, ok, err)
	}
	// Assets live under the doc prefix, so DeleteDoc must purge them too.
	if _, err := bs.PutDoc(ctx, "assetslug", 1, "<html></html>"); err != nil {
		t.Fatal(err)
	}
	if err := bs.DeleteDoc(ctx, "assetslug"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := bs.GetAsset(ctx, "assetslug", "deadbeef"); ok {
		t.Fatal("asset survived DeleteDoc; want purged with the doc prefix")
	}
	// DeleteAsset on an absent asset is not an error.
	if err := bs.DeleteAsset(ctx, "assetslug", "deadbeef"); err != nil {
		t.Fatalf("DeleteAsset absent = %v; want nil", err)
	}
}
