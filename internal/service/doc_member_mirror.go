package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// mysqlErrDupEntry is MySQL's ER_DUP_ENTRY (1062): a unique/primary-key
// collision. It is the ONLY error InsertDirectGrantIfAbsent treats as "row
// already exists"; every other driver/DB error propagates so reconcile retains
// the metadata instead of clearing it on a suppressed failure.
const mysqlErrDupEntry uint16 = 1062

// doc_member.role integer encoding. These MUST match the docs-backend's own
// doc_member.role values (same MySQL table, same database) so a row this
// service writes reads back with the same meaning on the backend and vice
// versa. Startup must first verify docs_metadata.role_encoding=append-v1; the verifier is
// read-only and never recodes role rows. The encoding is NOT
// capability-ordered (admin is 3, not the largest value); capability order
// (None<Read<Comment<Edit<Manage) is derived only via the explicit
// CapabilityForDocRole / roleCodeToLabel switches, never by numeric compare on
// the stored value.
const (
	// DocMemberRoleReader is the backend's reader encoding → CapRead.
	DocMemberRoleReader = 1
	// DocMemberRoleWriter is the backend's writer encoding → CapEdit
	// (AI/publish/draft) but not member management.
	DocMemberRoleWriter = 2
	// DocMemberRoleAdmin is the backend's admin encoding → CapManage (full
	// management). bestCred consumes it to short-circuit CapManage when the
	// caller's owner uid holds an admin row (plan③ A3②); the DB-level admin
	// guards bind this constant by equality (never an ordered compare), so the
	// mid-range value is safe.
	DocMemberRoleAdmin = 3
	// DocMemberRoleCommenter is the backend's commenter encoding → CapComment
	// (comment/react and edit/delete own comments).
	DocMemberRoleCommenter = 4
)

// ErrDocMemberAdminGuard is returned by DeleteGrant when the DB-level guard
// (WHERE role<>?, bound to DocMemberRoleAdmin) refuses the delete because a
// concurrent backfill promoted the row to admin between the caller's probe and
// the DELETE. Callers should translate this into their domain-level "protected"
// error (grants.RemoveGrant turns it into ErrGrantProtected).
var ErrDocMemberAdminGuard = errors.New("doc_member: refuse to modify admin row")

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
	DocIDBySlug(ctx context.Context, slug, spaceID string) (string, bool, error)
	// TitleBySlug reads the display title registered for slug in doc_meta.
	// spaceID scopes the lookup the same way DocIDBySlug does; ok=false when
	// the slug is unregistered (or the row is status=0). Display-only: callers
	// must treat errors as "keep the local title", never as a failure.
	TitleBySlug(ctx context.Context, slug, spaceID string) (string, bool, error)
	UpsertDirectGrant(ctx context.Context, docID, uid string, role int, grantedBy string) error
	// InsertDirectGrantIfAbsent inserts a direct grant only when no row exists
	// for (docID,uid); it NEVER updates an existing row and bumps
	// permission_epoch only on an actual insert. inserted=false means a row
	// already existed (any role) and was left untouched. Used by reconcile so a
	// gap-migration cannot clobber a concurrently-written authoritative row.
	InsertDirectGrantIfAbsent(ctx context.Context, docID, uid string, role int, grantedBy string) (inserted bool, err error)
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
//
// yujiawei round-4 P2 race guard: when the caller writes a non-admin role we
// preserve any existing admin row rather than downgrading it — a
// concurrent backfill can promote a row between the caller's probe and this
// write, and clobbering that admin down to a lesser role silently strips the
// author's capability. The ON DUPLICATE KEY UPDATE branch keeps the existing
// role/granted_by whenever the pre-image is admin. When the caller IS
// writing admin (e.g. an M1 owner backfill) we skip the guard so the promote
// still lands.
func (m *MySQLDocMemberMirror) UpsertDirectGrant(ctx context.Context, docID, uid string, role int, grantedBy string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin doc_member upsert: %w", err)
	}
	var insertSQL string
	if role == DocMemberRoleAdmin {
		insertSQL = `INSERT INTO doc_member (doc_id, uid, role, granted_by, source, invite_token)
		 VALUES (?,?,?,?,1,'')
		 ON DUPLICATE KEY UPDATE role=VALUES(role), granted_by=VALUES(granted_by)`
		if _, err := tx.ExecContext(ctx, insertSQL, docID, uid, role, grantedBy); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("upsert doc_member: %w", err)
		}
	} else {
		// Preserve an existing admin row (bind DocMemberRoleAdmin, do not hardcode
		// the numeric encoding) so a concurrent backfill promotion is never
		// clobbered down to a lesser role.
		insertSQL = `INSERT INTO doc_member (doc_id, uid, role, granted_by, source, invite_token)
		 VALUES (?,?,?,?,1,'')
		 ON DUPLICATE KEY UPDATE
		   role       = IF(role = ?, role, VALUES(role)),
		   granted_by = IF(role = ?, granted_by, VALUES(granted_by))`
		if _, err := tx.ExecContext(ctx, insertSQL, docID, uid, role, grantedBy, DocMemberRoleAdmin, DocMemberRoleAdmin); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("upsert doc_member: %w", err)
		}
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

// InsertDirectGrantIfAbsent inserts a direct grant only when no row exists for
// (docID,uid) and bumps permission_epoch only on an actual insert. It NEVER
// updates an existing row (any role), so a reconcile gap-migration cannot
// clobber a concurrently-written authoritative reader/commenter.
//
// It uses an ordinary INSERT (not INSERT IGNORE, which would also swallow FK /
// data-type errors and report RowsAffected=0, causing reconcile to clear the
// metadata after a real failure). A duplicate-key collision (ER_DUP_ENTRY 1062)
// is the single "already exists" signal (inserted=false, err=nil); any other
// error is returned so the caller retains the entry for retry.
func (m *MySQLDocMemberMirror) InsertDirectGrantIfAbsent(ctx context.Context, docID, uid string, role int, grantedBy string) (bool, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin doc_member insert-if-absent: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO doc_member (doc_id, uid, role, granted_by, source, invite_token)
		 VALUES (?,?,?,?,1,'')`,
		docID, uid, role, grantedBy)
	if err != nil {
		_ = tx.Rollback()
		var myErr *mysql.MySQLError
		if errors.As(err, &myErr) && myErr.Number == mysqlErrDupEntry {
			// Row already exists: no state change; caller may clear the entry.
			return false, nil
		}
		return false, fmt.Errorf("insert-if-absent doc_member: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE doc_meta SET permission_epoch=permission_epoch+1 WHERE doc_id=?",
		docID); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("bump doc_meta permission_epoch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit doc_member insert-if-absent: %w", err)
	}
	return true, nil
}

// DeleteGrant removes a doc_member row and bumps permission_epoch.
//
// yujiawei round-4 P2 race guard: the DELETE carries WHERE role<>? (bound to
// DocMemberRoleAdmin) so a row promoted to admin between the caller's probe and
// this call is not silently deleted. Affected=0 with the row still present (probe returned
// hit → row existed) means the guard kicked in; we return
// ErrDocMemberAdminGuard so callers can surface a protected-row error and
// skip the permission_epoch bump.
//
// yujiawei round-5 P2-α: (a) surface RowsAffected / probe SELECT errors
// instead of swallowing them — a transient DB error must not look like
// "already absent"; (b) bump permission_epoch only when a row was actually
// deleted, otherwise a no-op DELETE (row absent or concurrently already
// removed) needlessly invalidates every live connection's auth cache.
func (m *MySQLDocMemberMirror) DeleteGrant(ctx context.Context, docID, uid string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin doc_member delete: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		"DELETE FROM doc_member WHERE doc_id=? AND uid=? AND role<>?",
		docID, uid, DocMemberRoleAdmin)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete doc_member: %w", err)
	}
	affected, aerr := res.RowsAffected()
	if aerr != nil {
		_ = tx.Rollback()
		return fmt.Errorf("rows affected doc_member delete: %w", aerr)
	}
	if affected == 0 {
		// Distinguish "already absent" from "row is admin (guard tripped)".
		// A follow-up SELECT under the same tx snapshot decides.
		var role int
		qerr := tx.QueryRowContext(ctx,
			"SELECT role FROM doc_member WHERE doc_id=? AND uid=?",
			docID, uid).Scan(&role)
		switch {
		case qerr == nil && role == DocMemberRoleAdmin:
			_ = tx.Rollback()
			return ErrDocMemberAdminGuard
		case qerr != nil && !errors.Is(qerr, sql.ErrNoRows):
			_ = tx.Rollback()
			return fmt.Errorf("probe doc_member after guarded delete: %w", qerr)
		}
		// Row was absent (or non-admin and vanished under concurrent DELETE):
		// no state changed, skip the epoch bump. Commit an empty tx so we
		// still release DB resources cleanly.
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit no-op doc_member delete: %w", err)
		}
		return nil
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

// spaceIDCtxKey carries the requesting bot's space_id into the service layer
// so slug→doc_id resolution can be space-scoped. Written by the transport
// bot-auth middleware; read via SpaceIDFromContext.
type spaceIDCtxKey struct{}

// ContextWithSpaceID returns a context carrying spaceID for service-layer
// slug resolution. An empty spaceID stores nothing (callers then fall back to
// meta provenance / legacy unfiltered lookups).
func ContextWithSpaceID(ctx context.Context, spaceID string) context.Context {
	if spaceID == "" {
		return ctx
	}
	return context.WithValue(ctx, spaceIDCtxKey{}, spaceID)
}

// SpaceIDFromContext returns the space_id stashed by ContextWithSpaceID, or ""
// when absent.
func SpaceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(spaceIDCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// spaceIDForDoc derives the space scope for a slug lookup, in priority order:
// (a) the request context (bot-authenticated requests carry their space); (b)
// the slug's own persisted registration provenance when meta is already
// fetched; (c) "" — the legacy unfiltered lookup, kept so degraded /
// single-node / bot-without-space paths do not regress.
func spaceIDForDoc(ctx context.Context, meta *storage.DocMeta) string {
	if id := SpaceIDFromContext(ctx); id != "" {
		return id
	}
	if meta != nil {
		_, spaceID, _, _ := meta.PublishProvenance()
		return spaceID
	}
	return ""
}

// DocIDBySlug resolves a doc_id from its octo-doc slug, returning ok=false when
// the slug is not registered in doc_meta (mirror then skips silently). A slug
// is unique only WITHIN a space (uk_octo_doc_slug(space_id, octo_doc_slug)),
// so when spaceID != "" the lookup is space-scoped; spaceID == "" keeps the
// exact legacy unfiltered behavior for degraded/single-node paths.
func (m *MySQLDocMemberMirror) DocIDBySlug(ctx context.Context, slug, spaceID string) (string, bool, error) {
	query := "SELECT doc_id, permission_epoch FROM doc_meta WHERE octo_doc_slug=? AND status<>0"
	args := []any{slug}
	if spaceID != "" {
		query += " AND space_id=?"
		args = append(args, spaceID)
	}
	query += " LIMIT 1"
	var docID string
	var epoch sql.NullInt64
	err := m.db.QueryRowContext(ctx, query, args...).Scan(&docID, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve doc_meta by slug: %w", err)
	}
	return docID, true, nil
}

// TitleBySlug reads doc_meta.title for slug with the same space scoping and
// status<>0 guard as DocIDBySlug. ok=false when the slug is unregistered.
// Display-only: any error surfaces to the caller, which must fall back to its
// local title rather than fail the request.
func (m *MySQLDocMemberMirror) TitleBySlug(ctx context.Context, slug, spaceID string) (string, bool, error) {
	query := "SELECT title FROM doc_meta WHERE octo_doc_slug=? AND status<>0"
	args := []any{slug}
	if spaceID != "" {
		query += " AND space_id=?"
		args = append(args, spaceID)
	}
	query += " LIMIT 1"
	var title string
	err := m.db.QueryRowContext(ctx, query, args...).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve doc_meta title by slug: %w", err)
	}
	return title, true, nil
}

// RoleByDocUID returns the role (doc_member.role) uid holds on docID; ok=false
// when the uid has no row. Used by bestCred (via CapabilityForDocRole) to derive
// the caller's capability without touching meta.grants (plan③ A3/A4).
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
