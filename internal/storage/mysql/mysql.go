// Package mysql implements storage.MetadataStore on MySQL.
package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/core"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// Schema is the canonical MySQL DDL, applied at open and by the migrate command.
// Indexed identity columns are bounded because MySQL cannot use unbounded TEXT as
// a full primary key.
const Schema = `
CREATE TABLE IF NOT EXISTS meta (
    slug       VARCHAR(255) PRIMARY KEY,
    json       JSON         NOT NULL,
    updated_at BIGINT       NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
CREATE TABLE IF NOT EXISTS comments (
    slug       VARCHAR(255) PRIMARY KEY,
    json       JSON         NOT NULL,
    updated_at BIGINT       NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
CREATE TABLE IF NOT EXISTS sessions (
    sid        VARCHAR(512) PRIMARY KEY,
    json       JSON         NOT NULL,
    expires_at BIGINT       NOT NULL,
    INDEX sessions_expires_at_idx (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
CREATE TABLE IF NOT EXISTS tokens (
    token      VARCHAR(512) PRIMARY KEY,
    json       JSON         NOT NULL,
    created_at BIGINT       NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
CREATE TABLE IF NOT EXISTS assets (
    slug          VARCHAR(255) NOT NULL,
    sha256        VARCHAR(64)  NOT NULL,
    mime          TEXT         NOT NULL,
    size          BIGINT       NOT NULL,
    original_name TEXT         NOT NULL,
    created       TEXT         NOT NULL,
    PRIMARY KEY (slug, sha256),
    INDEX assets_slug_idx (slug)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
`

// Store is a MySQL-backed MetadataStore.
type Store struct {
	db     *sql.DB
	lockDB *sql.DB
}

var _ storage.MetadataStore = (*Store)(nil)

const maxIdentityChars = 512

// Open connects to MySQL using dsn, applies the schema, and returns a ready store.
func Open(ctx context.Context, dsn string, poolMax int) (*Store, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	formattedDSN := cfg.FormatDSN()
	db, err := openDB(ctx, formattedDSN, poolMax)
	if err != nil {
		return nil, err
	}
	if err := applySchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	lockDB, err := openDB(ctx, formattedDSN, poolMax)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect mysql (lock pool): %w", err)
	}
	return &Store{db: db, lockDB: lockDB}, nil
}

// Migrate applies the schema without keeping a store handle.
func Migrate(ctx context.Context, dsn string) error {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	db, err := openDB(ctx, cfg.FormatDSN(), 0)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := applySchema(ctx, db); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

func openDB(ctx context.Context, dsn string, poolMax int) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("connect mysql: %w", err)
	}
	if poolMax > 0 {
		db.SetMaxOpenConns(poolMax)
		db.SetMaxIdleConns(poolMax)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return db, nil
}

func applySchema(ctx context.Context, db *sql.DB) error {
	for _, stmt := range strings.Split(Schema, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	for _, stmt := range []string{
		"ALTER TABLE sessions MODIFY COLUMN sid VARCHAR(512) NOT NULL",
		"ALTER TABLE tokens MODIFY COLUMN token VARCHAR(512) NOT NULL",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func validateIdentity(field, value string) error {
	if utf8.RuneCountInString(value) > maxIdentityChars {
		return fmt.Errorf("%s exceeds %d characters", field, maxIdentityChars)
	}
	return nil
}

// --- meta ---

// GetMeta implements storage.MetadataStore.
func (s *Store) GetMeta(ctx context.Context, slug string) (*storage.DocMeta, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT json FROM meta WHERE slug=?", slug).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
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
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO meta(slug,json,updated_at) VALUES(?,?,?)
		 ON DUPLICATE KEY UPDATE json=VALUES(json), updated_at=VALUES(updated_at)`,
		slug, raw, nowMillis())
	if err != nil {
		return fmt.Errorf("put meta %q: %w", slug, err)
	}
	return nil
}

// DeleteMeta implements storage.MetadataStore.
func (s *Store) DeleteMeta(ctx context.Context, slug string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM meta WHERE slug=?", slug); err != nil {
		return fmt.Errorf("delete meta %q: %w", slug, err)
	}
	return nil
}

// ListMeta implements storage.MetadataStore.
func (s *Store) ListMeta(ctx context.Context) ([]storage.MetaEntry, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT slug, json FROM meta ORDER BY slug")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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
	err := s.db.QueryRowContext(ctx, "SELECT json FROM comments WHERE slug=?", slug).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []core.Comment{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []core.Comment
	if err := json.Unmarshal(raw, &list); err != nil {
		return []core.Comment{}, nil
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
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO comments(slug,json,updated_at) VALUES(?,?,?)
		 ON DUPLICATE KEY UPDATE json=VALUES(json), updated_at=VALUES(updated_at)`,
		slug, raw, nowMillis())
	if err != nil {
		return fmt.Errorf("put comments %q: %w", slug, err)
	}
	return nil
}

// DeleteComments implements storage.MetadataStore.
func (s *Store) DeleteComments(ctx context.Context, slug string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM comments WHERE slug=?", slug); err != nil {
		return fmt.Errorf("delete comments %q: %w", slug, err)
	}
	return nil
}

// --- sessions ---

// GetSession implements storage.MetadataStore.
func (s *Store) GetSession(ctx context.Context, sid string) (*storage.Session, error) {
	var raw []byte
	var expiresAt int64
	err := s.db.QueryRowContext(ctx, "SELECT json, expires_at FROM sessions WHERE sid=?", sid).Scan(&raw, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if expiresAt < nowMillis() {
		_, _ = s.db.ExecContext(ctx, "DELETE FROM sessions WHERE sid=?", sid)
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
	if err := validateIdentity("session id", sid); err != nil {
		return err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	exp := nowMillis() + int64(ttlSeconds)*1000
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(sid,json,expires_at) VALUES(?,?,?)
		 ON DUPLICATE KEY UPDATE json=VALUES(json), expires_at=VALUES(expires_at)`,
		sid, raw, exp); err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at < ?", nowMillis())
	return nil
}

// DeleteSession implements storage.MetadataStore.
func (s *Store) DeleteSession(ctx context.Context, sid string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE sid=?", sid)
	return err
}

// --- tokens ---

// GetToken implements storage.MetadataStore.
func (s *Store) GetToken(ctx context.Context, token string) (*storage.TokenRecord, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT json FROM tokens WHERE token=?", token).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
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
	if err := validateIdentity("token", token); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO tokens(token,json,created_at) VALUES(?,?,?)
		 ON DUPLICATE KEY UPDATE token=token`,
		token, raw, nowMillis())
	return err
}

// AnyToken implements storage.MetadataStore.
func (s *Store) AnyToken(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tokens").Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// --- assets ---

// PutAssetMeta implements storage.MetadataStore.
func (s *Store) PutAssetMeta(ctx context.Context, meta storage.AssetMeta) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO assets(slug,sha256,mime,size,original_name,created) VALUES(?,?,?,?,?,?)
		 ON DUPLICATE KEY UPDATE mime=VALUES(mime), size=VALUES(size), original_name=VALUES(original_name)`,
		meta.Slug, meta.SHA256, meta.MIME, meta.Size, meta.OriginalName, meta.Created)
	if err != nil {
		return fmt.Errorf("put asset meta %q/%q: %w", meta.Slug, meta.SHA256, err)
	}
	return nil
}

// GetAssetMeta implements storage.MetadataStore.
func (s *Store) GetAssetMeta(ctx context.Context, slug, sha256 string) (*storage.AssetMeta, error) {
	var m storage.AssetMeta
	err := s.db.QueryRowContext(ctx,
		"SELECT slug,sha256,mime,size,original_name,created FROM assets WHERE slug=? AND sha256=?", slug, sha256).
		Scan(&m.Slug, &m.SHA256, &m.MIME, &m.Size, &m.OriginalName, &m.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListAssetMeta implements storage.MetadataStore.
func (s *Store) ListAssetMeta(ctx context.Context, slug string) ([]storage.AssetMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT slug,sha256,mime,size,original_name,created FROM assets WHERE slug=? ORDER BY sha256", slug)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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
	if _, err := s.db.ExecContext(ctx, "DELETE FROM assets WHERE slug=? AND sha256=?", slug, sha256); err != nil {
		return fmt.Errorf("delete asset meta %q/%q: %w", slug, sha256, err)
	}
	return nil
}

// ListAssetSlugs implements storage.MetadataStore.
func (s *Store) ListAssetSlugs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT DISTINCT slug FROM assets ORDER BY slug")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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

// Close releases the connection pool.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.lockDB != nil {
		if lockErr := s.lockDB.Close(); err == nil {
			err = lockErr
		}
	}
	return err
}

// DB exposes the metadata pool for MySQL-only integrations that share octo_docs.
func (s *Store) DB() *sql.DB { return s.db }

// Locker returns a per-key distributed locker backed by MySQL named locks.
func (s *Store) Locker() sluglock.Locker {
	return &advisoryLocker{db: s.lockDB}
}

// Health verifies the database is reachable.
func (s *Store) Health(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("mysql ping: %w", err)
	}
	return nil
}

// TruncateAll removes every row from all tables. Intended for tests.
func (s *Store) TruncateAll(ctx context.Context) error {
	for _, table := range []string{"meta", "comments", "sessions", "tokens", "assets"} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	return nil
}
