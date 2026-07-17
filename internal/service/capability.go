package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// Capability is a viewer's access level for a specific document.
type Capability int

const (
	// CapNone means the credential grants no access to the doc → treat as absent.
	CapNone Capability = iota
	// CapReader can read published versions and comment/react, via a share code.
	CapReader
	// CapAuthor can do everything (read incl. drafts, publish, promote, delete,
	// generate/rotate codes) — the holder of a write token.
	CapAuthor
)

// shareExtraKey is the DocMeta.Extra key holding the per-doc share code hash.
const shareExtraKey = "share"

// hashCode returns the hex sha256 of a share code. Only the hash is persisted so
// a leaked metadata dump doesn't leak read access.
func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// CapabilityFor resolves the access level a credential grants for a slug. The
// credential is compared (constant-time) against the doc's stored share-code
// hash for CapReader; otherwise CapNone. An empty credential is CapNone.
//
// Author is NOT granted here: ownership is by creator uid (see bestCred), not by
// any bearer credential. Write tokens are no longer an auth source.
func (s *AuthService) CapabilityFor(ctx context.Context, slug, cred string) (Capability, error) {
	if cred == "" {
		return CapNone, nil
	}
	// Reader: the credential matches this doc's share code.
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return CapNone, err
	}
	if wantHash := shareCodeHash(meta); wantHash != "" {
		if constantTimeEqual(hashCode(cred), wantHash) {
			return CapReader, nil
		}
	}
	return CapNone, nil
}

// shareCodeHash extracts the stored share-code hash from meta, or "".
func shareCodeHash(meta *storage.DocMeta) string {
	if meta == nil || meta.Extra == nil {
		return ""
	}
	share, ok := meta.Extra[shareExtraKey].(map[string]any)
	if !ok {
		return ""
	}
	h, _ := share["code_hash"].(string)
	return h
}

// GenerateCode mints a new share code for a slug, stores its hash in meta, and
// returns the plaintext (shown once). Requires the doc to exist.
func (s *AuthService) GenerateCode(ctx context.Context, slug string) (string, error) {
	code := NewShareCode()
	err := s.lock.With(ctx, slug, func() error {
		meta, gerr := s.meta.GetMeta(ctx, slug)
		if gerr != nil {
			return gerr
		}
		if meta == nil {
			return apperr.NotFound("no such doc: " + slug)
		}
		extra := map[string]any{}
		maps.Copy(extra, meta.Extra)
		extra[shareExtraKey] = map[string]any{
			"code_hash":  hashCode(code),
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
		return s.meta.PutMeta(ctx, slug, storage.DocMeta{
			Slug: meta.Slug, Title: meta.Title, Versions: meta.Versions, Extra: extra,
		})
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

// RevokeCode removes the share code from a slug (existing links stop working).
func (s *AuthService) RevokeCode(ctx context.Context, slug string) error {
	return s.lock.With(ctx, slug, func() error {
		meta, gerr := s.meta.GetMeta(ctx, slug)
		if gerr != nil {
			return gerr
		}
		if meta == nil {
			return apperr.NotFound("no such doc: " + slug)
		}
		if _, has := meta.Extra[shareExtraKey]; !has {
			return nil
		}
		extra := map[string]any{}
		for k, v := range meta.Extra {
			if k != shareExtraKey {
				extra[k] = v
			}
		}
		if len(extra) == 0 {
			extra = nil
		}
		return s.meta.PutMeta(ctx, slug, storage.DocMeta{
			Slug: meta.Slug, Title: meta.Title, Versions: meta.Versions, Extra: extra,
		})
	})
}
