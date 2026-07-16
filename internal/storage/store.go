// Package storage defines the persistence contracts — MetadataStore for small
// structured records and BlobStore for immutable documents — plus a hash-based
// key helper shared by blob backends. The service layer depends only on these
// interfaces; no driver type (pgx row, S3 object) ever escapes an implementation.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/lml2468/octo-doc/internal/core"
)

// DocMeta is the metadata persisted per slug.
type DocMeta struct {
	Slug     string       `json:"slug"`
	Title    string       `json:"title"`
	Versions []VersionRef `json:"versions"`
	// Extra holds any additional caller-supplied metadata fields.
	Extra map[string]any `json:"-"`
}

// CreatorUIDExtraKey is the DocMeta.Extra key holding the creating bot's uid.
// Author capability is granted only to this uid (plus the superAdmin override),
// so the key is the single source of truth for doc ownership.
const CreatorUIDExtraKey = "creator_uid"

// CreatorUID returns the creating uid stored under Extra, or "" when absent
// (legacy docs / no creator recorded ⇒ nobody is author by ownership).
func (m *DocMeta) CreatorUID() string {
	if m == nil || m.Extra == nil {
		return ""
	}
	uid, _ := m.Extra[CreatorUIDExtraKey].(string)
	return uid
}

// GrantsExtraKey is the DocMeta.Extra key holding per-uid access grants: a
// map[uid]map[string]any{role, granted_by, created_at}. A granted uid gets that
// role in addition to the creator (author) and share-code (reader) paths.
const GrantsExtraKey = "grants"

// GrantRole returns the role explicitly granted to uid, or "" when uid has no
// grant. Nil-safe across every Extra layer so a legacy/absent grants map is "".
func (m *DocMeta) GrantRole(uid string) string {
	if m == nil || m.Extra == nil || uid == "" {
		return ""
	}
	grants, ok := m.Extra[GrantsExtraKey].(map[string]any)
	if !ok {
		return ""
	}
	entry, ok := grants[uid].(map[string]any)
	if !ok {
		return ""
	}
	role, _ := entry["role"].(string)
	return role
}

// DraftExtraKey is the DocMeta.Extra key holding the draft marker (a small map
// with updated_at). Its presence means a draft blob has been saved for the slug.
const DraftExtraKey = "draft"

// HasDraft reports whether a draft marker is recorded for the slug. Draft blobs
// live in the BlobStore (invisible to Versions); this flag is the meta-side
// signal that one exists, so a first-create gate can exclude slugs that already
// carry a draft even before any version is published.
func (m *DocMeta) HasDraft() bool {
	if m == nil || m.Extra == nil {
		return false
	}
	_, ok := m.Extra[DraftExtraKey]
	return ok
}

// VersionRef is one published version of a document.
type VersionRef struct {
	N       int     `json:"n"`
	Created *string `json:"created,omitempty"`
}

// Session is a signed-in viewer session. Role is set from X-Octo-Role (OCT-145
// 方案 C trust-header identity); empty for legacy cookie sessions. IsOwner
// grants owner-grade when Role == "superAdmin".
type Session struct {
	Login     string  `json:"login"`
	Name      string  `json:"name,omitempty"`
	AvatarURL *string `json:"avatar_url,omitempty"`
	Role      string  `json:"role,omitempty"`
	Created   string  `json:"created"`
	// OwnerUID is the user uid behind a bot session (bi.OwnerUID). In-memory
	// only (json:"-", never persisted): a bot acts on behalf of this user for
	// author-capability. Empty for real-user (trust-header) sessions, whose own
	// Login already is the author uid.
	OwnerUID string `json:"-"`
}

// TokenRecord is a provisioned write token.
type TokenRecord struct {
	Token   string `json:"token"`
	Created string `json:"created"`
	Label   string `json:"label,omitempty"`
}

// MetaEntry pairs a slug with its metadata for listing.
type MetaEntry struct {
	Slug string
	Meta DocMeta
}

// AssetMeta is the registry record for one content-addressed media asset owned by
// a document. The raw bytes live in the BlobStore keyed by (slug, SHA256); this
// is the metadata the serving path needs (MIME, size, display name). See
// docs/ASSETS.md.
type AssetMeta struct {
	Slug         string `json:"slug"`
	SHA256       string `json:"sha256"`
	MIME         string `json:"mime"`
	Size         int64  `json:"size"`
	OriginalName string `json:"original_name"`
	Created      string `json:"created"`
}

// MetadataStore persists doc metadata, comment logs, sessions, and write tokens.
// Implementations return plain Go values, never driver row types.
type MetadataStore interface {
	GetMeta(ctx context.Context, slug string) (*DocMeta, error)
	PutMeta(ctx context.Context, slug string, meta DocMeta) error
	DeleteMeta(ctx context.Context, slug string) error
	ListMeta(ctx context.Context) ([]MetaEntry, error)

	// GetComments always returns a slice; corrupt/absent values yield an empty one.
	GetComments(ctx context.Context, slug string) ([]core.Comment, error)
	PutComments(ctx context.Context, slug string, list []core.Comment) error
	DeleteComments(ctx context.Context, slug string) error

	GetSession(ctx context.Context, sid string) (*Session, error)
	PutSession(ctx context.Context, sid string, data Session, ttlSeconds int) error
	DeleteSession(ctx context.Context, sid string) error

	GetToken(ctx context.Context, token string) (*TokenRecord, error)
	PutToken(ctx context.Context, token string, rec TokenRecord) error
	AnyToken(ctx context.Context) (bool, error)

	// Asset metadata registry. Assets are content-addressed by SHA-256 and scoped
	// to a doc; (slug, sha256) is the identity, so re-registering identical bytes
	// is idempotent. See docs/ASSETS.md.
	PutAssetMeta(ctx context.Context, meta AssetMeta) error
	GetAssetMeta(ctx context.Context, slug, sha256 string) (*AssetMeta, error)
	ListAssetMeta(ctx context.Context, slug string) ([]AssetMeta, error)
	DeleteAssetMeta(ctx context.Context, slug, sha256 string) error
	// ListAssetSlugs returns every slug that has at least one asset row, ascending
	// and deduped. Used by asset GC to enumerate asset-bearing slugs independently
	// of DocMeta, so assets uploaded under a slug with no doc row are still reached.
	ListAssetSlugs(ctx context.Context) ([]string, error)

	// Health verifies the backend is reachable (readiness probe).
	Health(ctx context.Context) error

	Close() error
}

// BlobStore persists immutable HTML documents keyed by (slug, version).
type BlobStore interface {
	// PutDoc writes a version atomically (no half-writes) and returns its size.
	PutDoc(ctx context.Context, slug string, version int, html string) (size int64, err error)
	GetDoc(ctx context.Context, slug string, version int) (string, bool, error)
	HeadDoc(ctx context.Context, slug string, version int) (size int64, exists bool, err error)
	// ListVersions returns the versions present for a slug, ascending.
	ListVersions(ctx context.Context, slug string) ([]int, error)
	DeleteDoc(ctx context.Context, slug string) error

	// Draft is a single mutable, overwritable slot per slug, stored outside the
	// versioned key namespace so it never appears in ListVersions. It holds the
	// work-in-progress HTML before it is promoted to an immutable version.
	PutDraft(ctx context.Context, slug string, html string) (size int64, err error)
	GetDraft(ctx context.Context, slug string) (string, bool, error)
	DeleteDraft(ctx context.Context, slug string) error

	// Asset bytes are content-addressed at docs/<hashSlug>/assets/<sha256>. PutAsset
	// is idempotent (same key = same bytes). Assets live under the same per-slug
	// prefix as versions, so DeleteDoc removes them too. See docs/ASSETS.md.
	PutAsset(ctx context.Context, slug, sha256 string, data []byte) error
	GetAsset(ctx context.Context, slug, sha256 string) (data []byte, ok bool, err error)
	DeleteAsset(ctx context.Context, slug, sha256 string) error

	// Health verifies the backend is reachable (readiness probe).
	Health(ctx context.Context) error
}

// HashSlug hashes a slug to a fixed-length hex key safe as a path/prefix segment.
// Defined once so every blob backend shares the path-traversal defense.
func HashSlug(slug string) string {
	sum := sha256.Sum256([]byte(slug))
	return hex.EncodeToString(sum[:])[:32]
}
