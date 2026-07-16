package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const docMemberRoleReader = 1

// DocMemberMirror keeps rich-doc list membership in sync with doc-side grants.
type DocMemberMirror interface {
	DocIDBySlug(ctx context.Context, slug string) (string, bool, error)
	UpsertDirectGrant(ctx context.Context, docID, uid string, role int, grantedBy string) error
	DeleteGrant(ctx context.Context, docID, uid string) error
}

// MySQLDocMemberMirror mirrors doc-side grants into the rich-doc doc_member
// table (same MySQL database) so authorized users appear in the sidebar list.
type MySQLDocMemberMirror struct {
	db *sql.DB
}

// NewMySQLDocMemberMirror returns a mirror over db, or nil when db is nil (the
// no-op case for non-MySQL / unwired backends).
func NewMySQLDocMemberMirror(db *sql.DB) *MySQLDocMemberMirror {
	if db == nil {
		return nil
	}
	return &MySQLDocMemberMirror{db: db}
}

// UpsertDirectGrant upserts a direct doc_member row (role) and bumps the doc's
// permission_epoch so live connections re-evaluate access.
func (m *MySQLDocMemberMirror) UpsertDirectGrant(ctx context.Context, docID, uid string, role int, grantedBy string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin doc_member upsert: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO doc_member (doc_id, uid, role, granted_by, source, invite_token)
		 VALUES (?,?,?,?,1,'')
		 ON DUPLICATE KEY UPDATE role=VALUES(role), granted_by=VALUES(granted_by)`,
		docID, uid, role, grantedBy); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("upsert doc_member: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE doc_meta SET permission_epoch=permission_epoch+1 WHERE doc_id=?",
		docID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("bump doc_meta permission_epoch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit doc_member upsert: %w", err)
	}
	return nil
}

// DeleteGrant removes a doc_member row and bumps permission_epoch.
func (m *MySQLDocMemberMirror) DeleteGrant(ctx context.Context, docID, uid string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin doc_member delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM doc_member WHERE doc_id=? AND uid=?",
		docID, uid); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete doc_member: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE doc_meta SET permission_epoch=permission_epoch+1 WHERE doc_id=?",
		docID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("bump doc_meta permission_epoch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit doc_member delete: %w", err)
	}
	return nil
}

// DocIDBySlug resolves a doc_id from its octo-doc slug, returning ok=false when
// the slug is not registered in doc_meta (mirror then skips silently).
func (m *MySQLDocMemberMirror) DocIDBySlug(ctx context.Context, slug string) (string, bool, error) {
	var docID string
	var epoch sql.NullInt64
	err := m.db.QueryRowContext(ctx,
		"SELECT doc_id, permission_epoch FROM doc_meta WHERE octo_doc_slug=? AND status<>0 LIMIT 1",
		slug).Scan(&docID, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve doc_meta by slug: %w", err)
	}
	return docID, true, nil
}
