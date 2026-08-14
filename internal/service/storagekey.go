// Package service — storage key resolution. Every BlobStore call in this package
// must address objects by the document's *storage key* rather than by its
// client-supplied slug, so the public identity can later move to the
// backend-assigned doc id without touching a single stored object.
package service

import (
	"context"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// storageKeyOf returns the blob storage key for a document: the persisted
// Extra["storage_key"], falling back to slug when metadata is absent (a brand-new
// or blob-only slug) or carries no storage key (every document written before the
// key existed).
//
// The fallback is what keeps existing S3 object keys byte-identical: those
// documents were addressed by slug, and this returns exactly that slug.
func storageKeyOf(meta *storage.DocMeta, slug string) string {
	if key := meta.StorageKey(); key != "" {
		return key
	}
	return slug
}

// resolveStorageKey loads the document's metadata and returns its storage key.
// Used by call sites that do not already hold a *storage.DocMeta.
func resolveStorageKey(ctx context.Context, store storage.MetadataStore, slug string) (string, error) {
	meta, err := store.GetMeta(ctx, slug)
	if err != nil {
		return "", err
	}
	return storageKeyOf(meta, slug), nil
}
