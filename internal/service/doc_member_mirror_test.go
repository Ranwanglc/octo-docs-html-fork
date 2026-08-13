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
	"errors"
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
			octo_doc_slug VARCHAR(255),
			space_id VARCHAR(64) NOT NULL DEFAULT '',
			title VARCHAR(512) NOT NULL DEFAULT '',
			permission_epoch BIGINT NOT NULL DEFAULT 0,
			status INT NOT NULL DEFAULT 1,
			UNIQUE KEY uk_octo_doc_slug (space_id, octo_doc_slug)
		)`,
		`CREATE TABLE doc_member (
			doc_id VARCHAR(64),
			uid VARCHAR(128),
			role INT NOT NULL,
			granted_by VARCHAR(128),
			source INT NOT NULL DEFAULT 1,
			invite_token VARCHAR(128) NOT NULL DEFAULT '',
			PRIMARY KEY (doc_id, uid),
			CONSTRAINT fk_doc_member_doc FOREIGN KEY (doc_id)
				REFERENCES doc_meta (doc_id)
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

// InsertDirectGrantIfAbsent: a fresh row inserts, reports inserted=true, and
// bumps the epoch exactly once.
func TestInsertIfAbsentInsertsAndBumpsEpoch(t *testing.T) {
	db := mysqlMirrorTestDB(t)
	mirror := service.NewMySQLDocMemberMirror(db)
	docID := "docIfAbsentNew"
	before := seedEpoch(t, db, docID, 2)
	inserted, err := mirror.InsertDirectGrantIfAbsent(context.Background(), docID, "reader-1", service.DocMemberRoleReader, "reconcile")
	if err != nil || !inserted {
		t.Fatalf("insert-if-absent(new) = (%v,%v); want (true,nil)", inserted, err)
	}
	if after := currentEpoch(t, db, docID); after != before+1 {
		t.Fatalf("insert epoch: %d -> %d; want +1", before, after)
	}
}

// InsertDirectGrantIfAbsent must NEVER overwrite an existing row and must NOT
// bump the epoch when a row already exists (any role) — the P1 race guard: a
// concurrent authoritative writer/commenter is preserved.
func TestInsertIfAbsentNeverOverwritesOrBumps(t *testing.T) {
	db := mysqlMirrorTestDB(t)
	mirror := service.NewMySQLDocMemberMirror(db)
	docID := "docIfAbsentExisting"
	before := seedEpoch(t, db, docID, 5)
	if _, err := db.ExecContext(context.Background(),
		"INSERT INTO doc_member (doc_id, uid, role, granted_by) VALUES (?, ?, ?, ?)",
		docID, "user-1", service.DocMemberRoleWriter, "direct"); err != nil {
		t.Fatalf("seed writer: %v", err)
	}
	inserted, err := mirror.InsertDirectGrantIfAbsent(context.Background(), docID, "user-1", service.DocMemberRoleReader, "reconcile")
	if err != nil {
		t.Fatalf("insert-if-absent(existing) err = %v", err)
	}
	if inserted {
		t.Fatalf("inserted=true for existing row; must be false (no overwrite)")
	}
	var role int
	if err := db.QueryRowContext(context.Background(),
		"SELECT role FROM doc_member WHERE doc_id=? AND uid=?", docID, "user-1").Scan(&role); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if role != service.DocMemberRoleWriter {
		t.Fatalf("existing writer overwritten to %d; want writer preserved", role)
	}
	if after := currentEpoch(t, db, docID); after != before {
		t.Fatalf("no-op insert-if-absent bumped epoch: %d -> %d", before, after)
	}
}

// Non-duplicate insert failures must propagate so reconcile retains metadata.
func TestInsertIfAbsentPropagatesForeignKeyError(t *testing.T) {
	db := mysqlMirrorTestDB(t)
	mirror := service.NewMySQLDocMemberMirror(db)

	inserted, err := mirror.InsertDirectGrantIfAbsent(
		context.Background(), "missing-doc", "reader-1", service.DocMemberRoleReader, "reconcile",
	)
	if err == nil {
		t.Fatal("insert-if-absent(FK violation) err = nil; want non-duplicate error")
	}
	if inserted {
		t.Fatal("insert-if-absent(FK violation) inserted = true; want false")
	}
}

func TestRequireAppendRoleEncoding(t *testing.T) {
	db := mysqlMirrorTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE docs_metadata (
		meta_key VARCHAR(64) PRIMARY KEY, meta_value VARCHAR(64) NOT NULL)`); err != nil {
		t.Fatalf("create marker table: %v", err)
	}
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS docs_metadata") })

	for _, tc := range []struct {
		name, value string
		ok          bool
	}{
		{name: "append contract", value: service.DocRoleEncodingAppendV1, ok: true},
		{name: "ordered v2 rejected", value: "v2"},
		{name: "unknown rejected", value: "future-v9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, "DELETE FROM docs_metadata"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, "INSERT INTO docs_metadata(meta_key,meta_value) VALUES (?,?)", service.DocRoleEncodingKey, tc.value); err != nil {
				t.Fatal(err)
			}
			err := service.RequireAppendRoleEncoding(ctx, db)
			if tc.ok && err != nil {
				t.Fatalf("append-v1 rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("unsafe encoding %q accepted", tc.value)
			}
		})
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM docs_metadata"); err != nil {
		t.Fatal(err)
	}
	if err := service.RequireAppendRoleEncoding(ctx, db); err == nil {
		t.Fatal("missing marker accepted")
	}
	if err := service.RequireAppendRoleEncoding(ctx, nil); !errors.Is(err, service.ErrDocRoleEncodingUnverified) {
		t.Fatalf("nil db error = %v; want ErrDocRoleEncodingUnverified", err)
	}
}

// seedSlugDoc inserts a registered doc_meta row (slug + space + title) for the
// space-scoped slug resolution tests.
func seedSlugDoc(t *testing.T, db *sql.DB, docID, slug, spaceID, title string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		"INSERT INTO doc_meta (doc_id, octo_doc_slug, space_id, title, status) VALUES (?, ?, ?, ?, 1)",
		docID, slug, spaceID, title); err != nil {
		t.Fatalf("seed slug doc: %v", err)
	}
}

// A slug is unique only WITHIN a space (uk_octo_doc_slug(space_id,
// octo_doc_slug)): the same slug registered in two spaces must resolve to the
// matching row when the caller supplies the space, and the legacy empty-space
// lookup must still return one of them (degraded paths must not regress).
func TestDocIDBySlugIsSpaceScoped(t *testing.T) {
	db := mysqlMirrorTestDB(t)
	mirror := service.NewMySQLDocMemberMirror(db)
	ctx := context.Background()

	seedSlugDoc(t, db, "docSpaceA", "shared-slug", "space-a", "Title A")
	seedSlugDoc(t, db, "docSpaceB", "shared-slug", "space-b", "Title B")

	docID, ok, err := mirror.DocIDBySlug(ctx, "shared-slug", "space-a")
	if err != nil || !ok || docID != "docSpaceA" {
		t.Fatalf("DocIDBySlug(space-a) = (%q,%v,%v); want (docSpaceA,true,nil)", docID, ok, err)
	}
	docID, ok, err = mirror.DocIDBySlug(ctx, "shared-slug", "space-b")
	if err != nil || !ok || docID != "docSpaceB" {
		t.Fatalf("DocIDBySlug(space-b) = (%q,%v,%v); want (docSpaceB,true,nil)", docID, ok, err)
	}
	// Legacy empty space: unfiltered lookup still resolves (one of the rows).
	docID, ok, err = mirror.DocIDBySlug(ctx, "shared-slug", "")
	if err != nil || !ok || (docID != "docSpaceA" && docID != "docSpaceB") {
		t.Fatalf("DocIDBySlug(legacy empty space) = (%q,%v,%v); want one of the rows", docID, ok, err)
	}
	// A space with no registration of the slug must NOT see another space's row.
	_, ok, err = mirror.DocIDBySlug(ctx, "shared-slug", "space-c")
	if err != nil || ok {
		t.Fatalf("DocIDBySlug(space-c) = (%v,%v); want (false,nil)", ok, err)
	}
}

// TitleBySlug returns the registered display title, space-scoped, with the same
// status<>0 guard as DocIDBySlug.
func TestTitleBySlugSpaceScoped(t *testing.T) {
	db := mysqlMirrorTestDB(t)
	mirror := service.NewMySQLDocMemberMirror(db)
	ctx := context.Background()

	seedSlugDoc(t, db, "docTitleA", "titled-slug", "space-a", "Real Title A")
	seedSlugDoc(t, db, "docTitleB", "titled-slug", "space-b", "Real Title B")

	title, ok, err := mirror.TitleBySlug(ctx, "titled-slug", "space-a")
	if err != nil || !ok || title != "Real Title A" {
		t.Fatalf("TitleBySlug(space-a) = (%q,%v,%v); want (Real Title A,true,nil)", title, ok, err)
	}
	title, ok, err = mirror.TitleBySlug(ctx, "titled-slug", "space-b")
	if err != nil || !ok || title != "Real Title B" {
		t.Fatalf("TitleBySlug(space-b) = (%q,%v,%v); want (Real Title B,true,nil)", title, ok, err)
	}
	// Legacy empty space still resolves one of the rows' titles.
	title, ok, err = mirror.TitleBySlug(ctx, "titled-slug", "")
	if err != nil || !ok || (title != "Real Title A" && title != "Real Title B") {
		t.Fatalf("TitleBySlug(legacy empty space) = (%q,%v,%v); want one of the titles", title, ok, err)
	}
	_, ok, err = mirror.TitleBySlug(ctx, "titled-slug", "space-c")
	if err != nil || ok {
		t.Fatalf("TitleBySlug(space-c) = (%v,%v); want (false,nil)", ok, err)
	}
}

// status=0 rows (soft-deleted/unmounted) must never resolve — for either the
// doc_id or the title — regardless of space scoping.
func TestSlugResolutionSkipsStatusZeroRows(t *testing.T) {
	db := mysqlMirrorTestDB(t)
	mirror := service.NewMySQLDocMemberMirror(db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		"INSERT INTO doc_meta (doc_id, octo_doc_slug, space_id, title, status) VALUES (?, ?, ?, ?, 0)",
		"docDead", "dead-slug", "space-a", "Dead Title"); err != nil {
		t.Fatalf("seed status=0 row: %v", err)
	}
	for _, space := range []string{"space-a", ""} {
		_, ok, err := mirror.DocIDBySlug(ctx, "dead-slug", space)
		if err != nil || ok {
			t.Fatalf("DocIDBySlug(dead, space=%q) = (%v,%v); want (false,nil)", space, ok, err)
		}
		_, ok, err = mirror.TitleBySlug(ctx, "dead-slug", space)
		if err != nil || ok {
			t.Fatalf("TitleBySlug(dead, space=%q) = (%v,%v); want (false,nil)", space, ok, err)
		}
	}
}

// Unregistered slugs resolve to ok=false for both lookups (mirror skips).
func TestSlugResolutionUnregisteredSlug(t *testing.T) {
	db := mysqlMirrorTestDB(t)
	mirror := service.NewMySQLDocMemberMirror(db)
	ctx := context.Background()

	for _, space := range []string{"space-a", ""} {
		_, ok, err := mirror.DocIDBySlug(ctx, "ghost-slug", space)
		if err != nil || ok {
			t.Fatalf("DocIDBySlug(ghost, space=%q) = (%v,%v); want (false,nil)", space, ok, err)
		}
		_, ok, err = mirror.TitleBySlug(ctx, "ghost-slug", space)
		if err != nil || ok {
			t.Fatalf("TitleBySlug(ghost, space=%q) = (%v,%v); want (false,nil)", space, ok, err)
		}
	}
}
