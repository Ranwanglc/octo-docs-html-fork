// Package postgres implements storage.MetadataStore on PostgreSQL via pgx.
// Records are stored as JSONB; no pgx type escapes this package.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lml2468/octo-doc/internal/core"
	"github.com/lml2468/octo-doc/internal/platform/sluglock"
	"github.com/lml2468/octo-doc/internal/storage"
)

// Schema is the canonical DDL, applied at open and by the migrate command. All
// statements are idempotent (IF NOT EXISTS).
const Schema = `
CREATE TABLE IF NOT EXISTS meta (slug TEXT PRIMARY KEY, json JSONB NOT NULL, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS comments (slug TEXT PRIMARY KEY, json JSONB NOT NULL, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (sid TEXT PRIMARY KEY, json JSONB NOT NULL, expires_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS tokens (token TEXT PRIMARY KEY, json JSONB NOT NULL, created_at BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_expires_at_idx ON sessions (expires_at);
CREATE TABLE IF NOT EXISTS assets (
    slug          TEXT        NOT NULL,
    sha256        TEXT        NOT NULL,
    mime          TEXT        NOT NULL,
    size          BIGINT      NOT NULL,
    original_name TEXT        NOT NULL,
    created       TEXT        NOT NULL,
    PRIMARY KEY (slug, sha256)
);
CREATE INDEX IF NOT EXISTS assets_slug_idx ON assets (slug);
`

// Store is a PostgreSQL-backed MetadataStore.
type Store struct {
	pool *pgxpool.Pool
	// lockPool is a SEPARATE pool used only for advisory locks. Advisory locks are
	// session-scoped, so a lock is held for the whole critical section while the
	// locked work runs its own queries. If locks and queries shared one pool, N≥pool
	// concurrent lock-holders would consume every connection and no locked work
	// could get a connection to finish — a deadlock (observed). A dedicated lock
	// pool decouples them: a full lock pool just queues new waiters, while holders
	// always get query connections from `pool` to complete and release.
	lockPool *pgxpool.Pool
}

var _ storage.MetadataStore = (*Store)(nil)

// Open connects to PostgreSQL using databaseURL, applies the schema, and returns
// a ready store.
func Open(ctx context.Context, databaseURL string, poolMax int) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if poolMax > 0 {
		cfg.MaxConns = int32(poolMax)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, Schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Separate pool for advisory locks (see Store.lockPool). Sized like the main
	// pool so many slugs can be locked concurrently; a full lock pool just applies
	// backpressure to new waiters rather than starving locked work of query conns.
	lockCfg := cfg.Copy()
	lockPool, err := pgxpool.NewWithConfig(ctx, lockCfg)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect postgres (lock pool): %w", err)
	}
	return &Store{pool: pool, lockPool: lockPool}, nil
}

// Migrate applies the schema without keeping a store handle.
func Migrate(ctx context.Context, databaseURL string) error {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, Schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// --- meta ---

// GetMeta implements storage.MetadataStore.
func (s *Store) GetMeta(ctx context.Context, slug string) (*storage.DocMeta, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, "SELECT json FROM meta WHERE slug=$1", slug).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m storage.DocMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// PutMeta implements storage.MetadataStore.
func (s *Store) PutMeta(ctx context.Context, slug string, meta storage.DocMeta) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal meta %q: %w", slug, err)
	}
	_, err = s.pool.Exec(ctx,
		"INSERT INTO meta(slug,json,updated_at) VALUES($1,$2,$3) ON CONFLICT(slug) DO UPDATE SET json=$2, updated_at=$3",
		slug, raw, nowMillis())
	if err != nil {
		return fmt.Errorf("put meta %q: %w", slug, err)
	}
	return nil
}

// DeleteMeta implements storage.MetadataStore.
func (s *Store) DeleteMeta(ctx context.Context, slug string) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM meta WHERE slug=$1", slug); err != nil {
		return fmt.Errorf("delete meta %q: %w", slug, err)
	}
	return nil
}

// ListMeta implements storage.MetadataStore.
func (s *Store) ListMeta(ctx context.Context) ([]storage.MetaEntry, error) {
	rows, err := s.pool.Query(ctx, "SELECT slug, json FROM meta ORDER BY slug")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.MetaEntry
	for rows.Next() {
		var slug string
		var raw []byte
		if err := rows.Scan(&slug, &raw); err != nil {
			return nil, err
		}
		var m storage.DocMeta
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		out = append(out, storage.MetaEntry{Slug: slug, Meta: m})
	}
	return out, rows.Err()
}

// --- comments ---

// GetComments implements storage.MetadataStore.
func (s *Store) GetComments(ctx context.Context, slug string) ([]core.Comment, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, "SELECT json FROM comments WHERE slug=$1", slug).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return []core.Comment{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []core.Comment
	if err := json.Unmarshal(raw, &list); err != nil {
		return []core.Comment{}, nil // corrupt → empty, matching safeParseList
	}
	if list == nil {
		return []core.Comment{}, nil
	}
	return list, nil
}

// PutComments implements storage.MetadataStore.
func (s *Store) PutComments(ctx context.Context, slug string, list []core.Comment) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return fmt.Errorf("marshal comments %q: %w", slug, err)
	}
	_, err = s.pool.Exec(ctx,
		"INSERT INTO comments(slug,json,updated_at) VALUES($1,$2,$3) ON CONFLICT(slug) DO UPDATE SET json=$2, updated_at=$3",
		slug, raw, nowMillis())
	if err != nil {
		return fmt.Errorf("put comments %q: %w", slug, err)
	}
	return nil
}

// DeleteComments implements storage.MetadataStore.
func (s *Store) DeleteComments(ctx context.Context, slug string) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM comments WHERE slug=$1", slug); err != nil {
		return fmt.Errorf("delete comments %q: %w", slug, err)
	}
	return nil
}

// --- sessions ---

// GetSession implements storage.MetadataStore.
func (s *Store) GetSession(ctx context.Context, sid string) (*storage.Session, error) {
	var raw []byte
	var expiresAt int64
	err := s.pool.QueryRow(ctx, "SELECT json, expires_at FROM sessions WHERE sid=$1", sid).Scan(&raw, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if expiresAt < nowMillis() {
		_, _ = s.pool.Exec(ctx, "DELETE FROM sessions WHERE sid=$1", sid)
		return nil, nil
	}
	var sess storage.Session
	if err := json.Unmarshal(raw, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

// PutSession implements storage.MetadataStore.
func (s *Store) PutSession(ctx context.Context, sid string, data storage.Session, ttlSeconds int) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	exp := nowMillis() + int64(ttlSeconds)*1000
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO sessions(sid,json,expires_at) VALUES($1,$2,$3) ON CONFLICT(sid) DO UPDATE SET json=$2, expires_at=$3",
		sid, raw, exp); err != nil {
		return err
	}
	_, _ = s.pool.Exec(ctx, "DELETE FROM sessions WHERE expires_at < $1", nowMillis())
	return nil
}

// DeleteSession implements storage.MetadataStore.
func (s *Store) DeleteSession(ctx context.Context, sid string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE sid=$1", sid)
	return err
}

// --- tokens ---

// GetToken implements storage.MetadataStore.
func (s *Store) GetToken(ctx context.Context, token string) (*storage.TokenRecord, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, "SELECT json FROM tokens WHERE token=$1", token).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec storage.TokenRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// PutToken implements storage.MetadataStore.
func (s *Store) PutToken(ctx context.Context, token string, rec storage.TokenRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		"INSERT INTO tokens(token,json,created_at) VALUES($1,$2,$3) ON CONFLICT(token) DO NOTHING",
		token, raw, nowMillis())
	return err
}

// AnyToken implements storage.MetadataStore.
func (s *Store) AnyToken(ctx context.Context) (bool, error) {
	var n int
	if err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM tokens").Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// --- assets ---

// PutAssetMeta implements storage.MetadataStore. Idempotent on (slug, sha256):
// re-registering identical bytes refreshes the display fields without erroring.
func (s *Store) PutAssetMeta(ctx context.Context, meta storage.AssetMeta) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO assets(slug,sha256,mime,size,original_name,created) VALUES($1,$2,$3,$4,$5,$6)
		 ON CONFLICT(slug,sha256) DO UPDATE SET mime=$3, size=$4, original_name=$5`,
		meta.Slug, meta.SHA256, meta.MIME, meta.Size, meta.OriginalName, meta.Created)
	if err != nil {
		return fmt.Errorf("put asset meta %q/%q: %w", meta.Slug, meta.SHA256, err)
	}
	return nil
}

// GetAssetMeta implements storage.MetadataStore.
func (s *Store) GetAssetMeta(ctx context.Context, slug, sha256 string) (*storage.AssetMeta, error) {
	var m storage.AssetMeta
	err := s.pool.QueryRow(ctx,
		"SELECT slug,sha256,mime,size,original_name,created FROM assets WHERE slug=$1 AND sha256=$2", slug, sha256).
		Scan(&m.Slug, &m.SHA256, &m.MIME, &m.Size, &m.OriginalName, &m.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListAssetMeta implements storage.MetadataStore.
func (s *Store) ListAssetMeta(ctx context.Context, slug string) ([]storage.AssetMeta, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT slug,sha256,mime,size,original_name,created FROM assets WHERE slug=$1 ORDER BY sha256", slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]storage.AssetMeta, 0)
	for rows.Next() {
		var m storage.AssetMeta
		if err := rows.Scan(&m.Slug, &m.SHA256, &m.MIME, &m.Size, &m.OriginalName, &m.Created); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteAssetMeta implements storage.MetadataStore.
func (s *Store) DeleteAssetMeta(ctx context.Context, slug, sha256 string) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM assets WHERE slug=$1 AND sha256=$2", slug, sha256); err != nil {
		return fmt.Errorf("delete asset meta %q/%q: %w", slug, sha256, err)
	}
	return nil
}

// ListAssetSlugs implements storage.MetadataStore.
func (s *Store) ListAssetSlugs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, "SELECT DISTINCT slug FROM assets ORDER BY slug")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		out = append(out, slug)
	}
	return out, rows.Err()
}

// Close releases the connection pools.
func (s *Store) Close() error {
	s.pool.Close()
	if s.lockPool != nil {
		s.lockPool.Close()
	}
	return nil
}

// Locker returns a per-key distributed locker backed by PostgreSQL advisory
// locks over this store's dedicated lock pool. Share one instance across services
// so publish, comment, and bootstrap serialize on the same slug across app
// instances.
func (s *Store) Locker() sluglock.Locker {
	return &advisoryLocker{pool: s.lockPool}
}

// Health verifies the database is reachable (used by the readiness probe).
func (s *Store) Health(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres ping: %w", err)
	}
	return nil
}

// TruncateAll removes every row from all tables. Intended for tests.
func (s *Store) TruncateAll(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, "TRUNCATE meta, comments, sessions, tokens, assets")
	return err
}
