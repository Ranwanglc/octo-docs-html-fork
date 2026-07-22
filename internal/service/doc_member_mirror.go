package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const docMemberRoleReader = 1

// DocMember is one row of the rich-doc doc_member table exposed to callers that
// need to enumerate a doc's direct grants (grants.ListGrants, A6). Fields map
// 1:1 to the columns AuthService actually consumes.
type DocMember struct {
	UID       string
	Role      int
	GrantedBy string
}

// DocMemberMirror keeps rich-doc list membership in sync with doc-side grants
// and lets the auth layer read that same table when deciding capability.
// RoleByDocUID / ListMembers replace the legacy meta.grants read path
// (plan③ A3/A4/A6) so grants have a single source of truth in doc_member.
type DocMemberMirror interface {
	DocIDBySlug(ctx context.Context, slug string) (string, bool, error)
	UpsertDirectGrant(ctx context.Context, docID, uid string, role int, grantedBy string) error
	DeleteGrant(ctx context.Context, docID, uid string) error
	RoleByDocUID(ctx context.Context, docID, uid string) (int, bool, error)
	ListMembers(ctx context.Context, docID string) ([]DocMember, error)
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

// RoleByDocUID returns the role (doc_member.role) uid holds on docID; ok=false
// when the uid has no row. Used by bestCred to decide owner-admin (role=3) and
// reader (role>=1) capability without touching meta.grants (plan③ A3/A4).
// No cache: doc_member is fast and any cache here would tie freshness of auth
// to permission_epoch invalidation logic we do not need to add.
func (m *MySQLDocMemberMirror) RoleByDocUID(ctx context.Context, docID, uid string) (int, bool, error) {
	var role int
	err := m.db.QueryRowContext(ctx,
		"SELECT role FROM doc_member WHERE doc_id=? AND uid=? LIMIT 1",
		docID, uid).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read doc_member role: %w", err)
	}
	return role, true, nil
}

// ListMembers returns every doc_member row for docID. Used by grants.ListGrants
// (plan③ A6) so the sidebar/API render off doc_member instead of meta.grants.
// Ordered by created_at then uid for stable rendering; no caller depends on it
// beyond that.
func (m *MySQLDocMemberMirror) ListMembers(ctx context.Context, docID string) ([]DocMember, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT uid, role, granted_by FROM doc_member
		 WHERE doc_id=? ORDER BY created_at ASC, uid ASC`,
		docID)
	if err != nil {
		return nil, fmt.Errorf("list doc_member: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DocMember
	for rows.Next() {
		var dm DocMember
		var grantedBy sql.NullString
		if err := rows.Scan(&dm.UID, &dm.Role, &grantedBy); err != nil {
			return nil, fmt.Errorf("scan doc_member row: %w", err)
		}
		dm.GrantedBy = grantedBy.String
		out = append(out, dm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate doc_member rows: %w", err)
	}
	return out, nil
}
