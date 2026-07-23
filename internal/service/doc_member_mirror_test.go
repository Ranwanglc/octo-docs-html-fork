package service_test

// yujiawei round-5 P2-α regression tests for DeleteGrant epoch/error semantics.
//
// These tests hit real MySQL via OCTO_TEST_MYSQL_DSN because the semantics
// under test (permission_epoch bump timing, RowsAffected error handling)
// only exist in the MySQL implementation — the in-memory fake mirror does
// not model doc_meta.permission_epoch. Skipped when the DSN is unset so CI
// without a database still passes; the storage-layer TestMySQLContract
// uses the same pattern.

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
)

// setupDocMemberMirrorTables provisions the docs-backend tables the mirror
// touches. The tables live in docs-backend in prod; here we drop and
// recreate for a hermetic per-test surface.
func setupDocMemberMirrorTables(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		"DROP TABLE IF EXISTS doc_member",
		"DROP TABLE IF EXISTS doc_meta",
		`CREATE TABLE doc_meta (
			doc_id VARCHAR(64) PRIMARY KEY,
			octree_doc_slug VARCHAR(255),
			permission_epoch BIGINT NOT NULL DEFAULT 0,
			status INT NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE doc_member (
			doc_id VARCHAR(64),
			uid VARCHAR(128),
			role INT NOT NULL,
			granted_by VARCHAR(128),
			PRIMARY KEY (doc_id, uid)
		)`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
}

func mysqlMirrorTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("OCTO_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set OCTO_TEST_MYSQL_DSN to run doc_member mirror tests")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	setupDocMemberMirrorTables(t, db)
	return db
}

// seedEpoch inserts a doc_meta row and returns its current permission_epoch.
func seedEpoch(t *testing.T, db *sql.DB, docID string, initialEpoch int) int {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		"INSERT INTO doc_meta (doc_id, permission_epoch, status) VALUES (?, ?, 1)",
		docID, initialEpoch); err != nil {
		t.Fatalf("seed doc_meta: %v", err)
	}
	var ep int
	if err := db.QueryRowContext(ctx,
		"SELECT permission_epoch FROM doc_meta WHERE doc_id=?", docID).Scan(&ep); err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	return ep
}

func currentEpoch(t *testing.T, db *sql.DB, docID string) int {
	t.Helper()
	var ep int
	if err := db.QueryRowContext(context.Background(),
		"SELECT permission_epoch FROM doc_meta WHERE doc_id=?", docID).Scan(&ep); err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	return ep
}

// yujiawei round-5 P2-α: DeleteGrant on an absent row must NOT bump
// permission_epoch (a no-op DELETE should not invalidate live auth caches).
func TestDeleteGrantNoEpochBumpOnAbsentRow(t *testing.T) {
	db := mysqlMirrorTestDB(t)
	mirror := service.NewMySQLDocMemberMirror(db)

	docID := "docP2a"
	before := seedEpoch(t, db, docID, 7)

	if err := mirror.DeleteGrant(context.Background(), docID, "ghost-uid"); err != nil {
		t.Fatalf("DeleteGrant(absent) err = %v; want nil", err)
	}
	after := currentEpoch(t, db, docID)
	if after != before {
		t.Fatalf("no-op DeleteGrant bumped epoch: %d -> %d", before, after)
	}
}

// Real delete of a reader row DOES bump the epoch (regression on the
// happy-path invariant while we're touching this code).
func TestDeleteGrantBumpsEpochOnRealDelete(t *testing.T) {
	db := mysqlMirrorTestDB(t)
	mirror := service.NewMySQLDocMemberMirror(db)

	docID := "docP2aHit"
	before := seedEpoch(t, db, docID, 3)
	if _, err := db.ExecContext(context.Background(),
		"INSERT INTO doc_member (doc_id, uid, role, granted_by) VALUES (?, ?, ?, ?)",
		docID, "reader-1", service.DocMemberRoleReader, "seed"); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	if err := mirror.DeleteGrant(context.Background(), docID, "reader-1"); err != nil {
		t.Fatalf("DeleteGrant(real) err = %v; want nil", err)
	}
	after := currentEpoch(t, db, docID)
	if after != before+1 {
		t.Fatalf("real DeleteGrant epoch: %d -> %d; want +1", before, after)
	}
}

// yujiawei round-5 P2-α: an admin row still trips the guard (regression),
// and the guard path does NOT bump epoch.
func TestDeleteGrantAdminGuardTripsNoEpochBump(t *testing.T) {
	db := mysqlMirrorTestDB(t)
	mirror := service.NewMySQLDocMemberMirror(db)

	docID := "docP2aAdmin"
	before := seedEpoch(t, db, docID, 9)
	if _, err := db.ExecContext(context.Background(),
		"INSERT INTO doc_member (doc_id, uid, role, granted_by) VALUES (?, ?, ?, ?)",
		docID, "owner-1", service.DocMemberRoleAdmin, "seed"); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	err := mirror.DeleteGrant(context.Background(), docID, "owner-1")
	if err == nil || err.Error() != service.ErrDocMemberAdminGuard.Error() {
		t.Fatalf("DeleteGrant(admin) err = %v; want ErrDocMemberAdminGuard", err)
	}
	after := currentEpoch(t, db, docID)
	if after != before {
		t.Fatalf("guard-tripped DeleteGrant bumped epoch: %d -> %d", before, after)
	}
}
