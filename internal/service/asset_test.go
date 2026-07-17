package service_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

// pngBytes is a minimal valid PNG signature + IHDR-ish header; enough for
// http.DetectContentType to classify it as image/png.
var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}

// TestAssetPutAcceptsSniffableAllowlistTypes guards that the default-allowlisted
// types http.DetectContentType alone gets wrong (SVG, AVIF, WAV, OGG) are
// accepted via the strengthened sniffer.
func TestAssetPutAcceptsSniffableAllowlistTypes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`), "image/svg+xml"},
		{"svg-bare", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), "image/svg+xml"},
		{"wav", []byte("RIFF\x24\x00\x00\x00WAVEfmt "), "audio/wav"},
		{"webp", []byte("RIFF\x24\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"ogg", []byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00"), "audio/ogg"},
		{"avif", []byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00"), "image/avif"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := newAssets(t, 1<<20, []string{c.want})
			res, err := svc.Put(ctx, "d", bytes.NewReader(c.data), c.name)
			if err != nil {
				t.Fatalf("Put(%s) rejected: %v", c.name, err)
			}
			if res.MIME != c.want {
				t.Errorf("MIME = %q; want %q", res.MIME, c.want)
			}
		})
	}
}

func newAssets(t *testing.T, maxBytes int64, allow []string) *service.AssetService {
	t.Helper()
	store := memory.New()
	locker := sluglock.NewMemory()
	return service.NewAssetService(store, store, locker, maxBytes, allow)
}

func TestAssetPutGetRoundTrip(t *testing.T) {
	svc := newAssets(t, 1<<20, []string{"image/png"})
	ctx := context.Background()

	res, err := svc.Put(ctx, "d", bytes.NewReader(pngBytes), "logo.png")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if res.MIME != "image/png" {
		t.Errorf("mime = %q, want image/png", res.MIME)
	}
	if res.Size != int64(len(pngBytes)) {
		t.Errorf("size = %d, want %d", res.Size, len(pngBytes))
	}
	if len(res.SHA256) != 64 {
		t.Errorf("sha256 = %q, want 64 hex chars", res.SHA256)
	}

	data, meta, err := svc.Get(ctx, "d", res.SHA256)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(data, pngBytes) {
		t.Error("round-tripped bytes differ")
	}
	if meta.OriginalName != "logo.png" || meta.MIME != "image/png" {
		t.Errorf("meta = %+v", meta)
	}
}

func TestAssetPutIsIdempotent(t *testing.T) {
	svc := newAssets(t, 1<<20, []string{"image/png"})
	ctx := context.Background()

	r1, err := svc.Put(ctx, "d", bytes.NewReader(pngBytes), "a.png")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.Put(ctx, "d", bytes.NewReader(pngBytes), "a.png")
	if err != nil {
		t.Fatal(err)
	}
	if r1.SHA256 != r2.SHA256 {
		t.Errorf("identical bytes gave different hashes: %s vs %s", r1.SHA256, r2.SHA256)
	}
	list, err := svc.List(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("dedupe failed: %d assets, want 1", len(list))
	}
}

func TestAssetPutRejectsOversize(t *testing.T) {
	svc := newAssets(t, 8, []string{"image/png"}) // cap below the payload
	_, err := svc.Put(context.Background(), "d", bytes.NewReader(pngBytes), "big.png")
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Status != 413 {
		t.Fatalf("want 413 apperr, got %v", err)
	}
}

func TestAssetPutRejectsDisallowedMIME(t *testing.T) {
	svc := newAssets(t, 1<<20, []string{"image/jpeg"}) // png not allowed
	_, err := svc.Put(context.Background(), "d", bytes.NewReader(pngBytes), "logo.png")
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Status != 400 || ae.Code != "unsupported_media_type" {
		t.Fatalf("want 400 unsupported_media_type, got %v", err)
	}
}

func TestAssetPutSniffsOverClientClaim(t *testing.T) {
	// A plain-text payload sniffs as text/plain regardless of the ".png" name, so
	// with only image/png allowed it must be rejected — the sniff wins, not the name.
	svc := newAssets(t, 1<<20, []string{"image/png"})
	_, err := svc.Put(context.Background(), "d", strings.NewReader("just text"), "evil.png")
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != "unsupported_media_type" {
		t.Fatalf("want unsupported_media_type, got %v", err)
	}
}

func TestAssetGetMissing(t *testing.T) {
	svc := newAssets(t, 1<<20, []string{"image/png"})
	_, _, err := svc.Get(context.Background(), "d", strings.Repeat("0", 64))
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("want 404, got %v", err)
	}
}

func TestAssetDeleteIsIdempotent(t *testing.T) {
	svc := newAssets(t, 1<<20, []string{"image/png"})
	ctx := context.Background()
	res, err := svc.Put(ctx, "d", bytes.NewReader(pngBytes), "a.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, "d", res.SHA256); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Second delete of the now-absent asset must not error.
	if err := svc.Delete(ctx, "d", res.SHA256); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if _, _, err := svc.Get(ctx, "d", res.SHA256); err == nil {
		t.Error("asset still present after delete")
	}
}

// TestDocRemovePurgesAssets verifies deleting a doc drops its asset metadata rows
// (not just the bytes), so nothing orphans if the slug is later reused.
func TestDocRemovePurgesAssets(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	comments := service.NewCommentService(store, locker)
	docs := service.NewDocService(store, store, comments, locker, "", 5<<20)
	assets := service.NewAssetService(store, store, locker, 1<<20, []string{"image/png"})
	ctx := context.Background()

	if _, err := docs.Publish(ctx, service.PublishInput{Slug: "d", HTML: "<html><body><p>x</p></body></html>"}); err != nil {
		t.Fatal(err)
	}
	if _, err := assets.Put(ctx, "d", bytes.NewReader(pngBytes), "a.png"); err != nil {
		t.Fatal(err)
	}
	if err := docs.Remove(ctx, "d"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	list, err := assets.List(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("asset metadata survived doc delete: %d rows", len(list))
	}
}
