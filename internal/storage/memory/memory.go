// Package memory is an in-process implementation of the storage interfaces, used
// by unit tests and the storage contract suite. It is not for production use.
package memory

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/lml2468/octo-doc/internal/core"
	"github.com/lml2468/octo-doc/internal/storage"
)

// Store implements both storage.MetadataStore and storage.BlobStore in memory.
type Store struct {
	mu        sync.RWMutex
	meta      map[string]storage.DocMeta
	comments  map[string][]core.Comment
	sessions  map[string]sessionEntry
	tokens    map[string]storage.TokenRecord
	blobs     map[string]string            // "slug\x00version" -> html
	assets    map[string][]byte            // "slug\x00sha256" -> bytes
	assetMeta map[string]storage.AssetMeta // "slug\x00sha256" -> meta
}

type sessionEntry struct {
	data      storage.Session
	expiresAt int64
}

// New returns an empty in-memory store.
func New() *Store {
	return &Store{
		meta:      map[string]storage.DocMeta{},
		comments:  map[string][]core.Comment{},
		sessions:  map[string]sessionEntry{},
		tokens:    map[string]storage.TokenRecord{},
		blobs:     map[string]string{},
		assets:    map[string][]byte{},
		assetMeta: map[string]storage.AssetMeta{},
	}
}

// --- MetadataStore ---

// cloneJSON returns a fully-independent copy of v by round-tripping through JSON —
// the same isolation the postgres store gets for free (it serializes to JSONB and
// re-parses). A shallow copy is not enough for the stored types: DocMeta.Extra
// holds nested map/slice values, and core.Comment has nested Events/Reactions and
// pointer fields that in-place op application mutates. The fake must match
// postgres to stay a faithful conformance store. These are plain data types, so a
// marshal failure is a programmer error — panic loudly rather than silently
// returning an aliasing copy (which would reintroduce the exact bug this prevents).
func cloneJSON[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("memory store: marshal for clone: " + err.Error())
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		panic("memory store: unmarshal for clone: " + err.Error())
	}
	return out
}

// GetMeta implements storage.MetadataStore.
func (s *Store) GetMeta(_ context.Context, slug string) (*storage.DocMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.meta[slug]
	if !ok {
		return nil, nil
	}
	c := cloneJSON(m)
	return &c, nil
}

// PutMeta implements storage.MetadataStore.
func (s *Store) PutMeta(_ context.Context, slug string, meta storage.DocMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta[slug] = cloneJSON(meta)
	return nil
}

// DeleteMeta implements storage.MetadataStore.
func (s *Store) DeleteMeta(_ context.Context, slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.meta, slug)
	return nil
}

// ListMeta implements storage.MetadataStore.
func (s *Store) ListMeta(_ context.Context) ([]storage.MetaEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]storage.MetaEntry, 0, len(s.meta))
	for slug, m := range s.meta {
		out = append(out, storage.MetaEntry{Slug: slug, Meta: cloneJSON(m)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// GetComments implements storage.MetadataStore.
func (s *Store) GetComments(_ context.Context, slug string) ([]core.Comment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.comments[slug]
	if list == nil {
		return []core.Comment{}, nil
	}
	return cloneJSON(list), nil
}

// PutComments implements storage.MetadataStore.
func (s *Store) PutComments(_ context.Context, slug string, list []core.Comment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.comments[slug] = cloneJSON(list)
	return nil
}

// DeleteComments implements storage.MetadataStore.
func (s *Store) DeleteComments(_ context.Context, slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.comments, slug)
	return nil
}

// GetSession implements storage.MetadataStore.
func (s *Store) GetSession(_ context.Context, sid string) (*storage.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[sid]
	if !ok {
		return nil, nil
	}
	if e.expiresAt < time.Now().UnixMilli() {
		delete(s.sessions, sid)
		return nil, nil
	}
	d := e.data
	return &d, nil
}

// PutSession implements storage.MetadataStore.
func (s *Store) PutSession(_ context.Context, sid string, data storage.Session, ttlSeconds int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sid] = sessionEntry{data: data, expiresAt: time.Now().UnixMilli() + int64(ttlSeconds)*1000}
	return nil
}

// DeleteSession implements storage.MetadataStore.
func (s *Store) DeleteSession(_ context.Context, sid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sid)
	return nil
}

// GetToken implements storage.MetadataStore.
func (s *Store) GetToken(_ context.Context, token string) (*storage.TokenRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tokens[token]
	if !ok {
		return nil, nil
	}
	return &t, nil
}

// PutToken implements storage.MetadataStore.
func (s *Store) PutToken(_ context.Context, token string, rec storage.TokenRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tokens[token]; exists {
		return nil // ON CONFLICT DO NOTHING
	}
	s.tokens[token] = rec
	return nil
}

// AnyToken implements storage.MetadataStore.
func (s *Store) AnyToken(_ context.Context) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tokens) > 0, nil
}

// assetKey namespaces asset records by slug and content hash.
func assetKey(slug, sha256 string) string { return slug + "\x00" + sha256 }

// PutAssetMeta implements storage.MetadataStore. Idempotent on (slug, sha256):
// re-registering identical bytes refreshes the display fields but PRESERVES the
// original Created timestamp, matching the postgres ON CONFLICT clause (which does
// not update created). This keeps the GC grace window anchored to first upload.
func (s *Store) PutAssetMeta(_ context.Context, meta storage.AssetMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := assetKey(meta.Slug, meta.SHA256)
	if existing, ok := s.assetMeta[key]; ok {
		meta.Created = existing.Created
	}
	s.assetMeta[key] = cloneJSON(meta)
	return nil
}

// GetAssetMeta implements storage.MetadataStore.
func (s *Store) GetAssetMeta(_ context.Context, slug, sha256 string) (*storage.AssetMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.assetMeta[assetKey(slug, sha256)]
	if !ok {
		return nil, nil
	}
	c := cloneJSON(m)
	return &c, nil
}

// ListAssetMeta implements storage.MetadataStore.
func (s *Store) ListAssetMeta(_ context.Context, slug string) ([]storage.AssetMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := slug + "\x00"
	out := make([]storage.AssetMeta, 0)
	for k, m := range s.assetMeta {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, cloneJSON(m))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SHA256 < out[j].SHA256 })
	return out, nil
}

// DeleteAssetMeta implements storage.MetadataStore.
func (s *Store) DeleteAssetMeta(_ context.Context, slug, sha256 string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.assetMeta, assetKey(slug, sha256))
	return nil
}

// ListAssetSlugs implements storage.MetadataStore.
func (s *Store) ListAssetSlugs(_ context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]struct{}{}
	for _, m := range s.assetMeta {
		seen[m.Slug] = struct{}{}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// Close implements storage.MetadataStore.
func (s *Store) Close() error { return nil }

// Health implements storage.MetadataStore and storage.BlobStore; the in-memory
// store is always reachable.
func (s *Store) Health(_ context.Context) error { return nil }

// --- BlobStore ---

func blobKey(slug string, version int) string {
	return slug + "\x00" + itoa(version)
}

// PutDoc implements storage.BlobStore.
func (s *Store) PutDoc(_ context.Context, slug string, version int, html string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[blobKey(slug, version)] = html
	return int64(len(html)), nil
}

// GetDoc implements storage.BlobStore.
func (s *Store) GetDoc(_ context.Context, slug string, version int) (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.blobs[blobKey(slug, version)]
	return h, ok, nil
}

// HeadDoc implements storage.BlobStore.
func (s *Store) HeadDoc(_ context.Context, slug string, version int) (int64, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.blobs[blobKey(slug, version)]
	if !ok {
		return 0, false, nil
	}
	return int64(len(h)), true, nil
}

// ListVersions implements storage.BlobStore.
func (s *Store) ListVersions(_ context.Context, slug string) ([]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := slug + "\x00"
	var out []int
	for k := range s.blobs {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			if n, ok := atoi(k[len(prefix):]); ok {
				out = append(out, n)
			}
		}
	}
	sort.Ints(out)
	return out, nil
}

// DeleteDoc implements storage.BlobStore. It also purges the slug's assets, since
// the S3 backend deletes them via the shared docs/<hash>/ prefix.
func (s *Store) DeleteDoc(_ context.Context, slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := slug + "\x00"
	for k := range s.blobs {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(s.blobs, k)
		}
	}
	for k := range s.assets {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(s.assets, k)
		}
	}
	return nil
}

// draftKey is the mutable draft slot. The "draft" suffix is not a number, so
// ListVersions's atoi skips it — it never appears as a version.
func draftKey(slug string) string { return slug + "\x00draft" }

// PutDraft implements storage.BlobStore.
func (s *Store) PutDraft(_ context.Context, slug string, html string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[draftKey(slug)] = html
	return int64(len(html)), nil
}

// GetDraft implements storage.BlobStore.
func (s *Store) GetDraft(_ context.Context, slug string) (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.blobs[draftKey(slug)]
	return h, ok, nil
}

// DeleteDraft implements storage.BlobStore.
func (s *Store) DeleteDraft(_ context.Context, slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blobs, draftKey(slug))
	return nil
}

// PutAsset implements storage.BlobStore. Content-addressed, so it copies the
// bytes and is idempotent on identical (slug, sha256).
func (s *Store) PutAsset(_ context.Context, slug, sha256 string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	s.assets[assetKey(slug, sha256)] = cp
	return nil
}

// GetAsset implements storage.BlobStore.
func (s *Store) GetAsset(_ context.Context, slug, sha256 string) ([]byte, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.assets[assetKey(slug, sha256)]
	if !ok {
		return nil, false, nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, true, nil
}

// DeleteAsset implements storage.BlobStore.
func (s *Store) DeleteAsset(_ context.Context, slug, sha256 string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.assets, assetKey(slug, sha256))
	return nil
}

// small int<->string helpers to avoid strconv churn in hot paths
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func atoi(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}
