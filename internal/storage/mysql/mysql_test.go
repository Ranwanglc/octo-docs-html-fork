package mysql_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/mysql"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/storagetest"
)

// TestMySQLContract runs the storage contract against a real MySQL when
// OCTO_TEST_MYSQL_DSN is set; otherwise it is skipped.
func TestMySQLContract(t *testing.T) {
	dsn := os.Getenv("OCTO_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set OCTO_TEST_MYSQL_DSN to run the MySQL contract test")
	}
	ctx := context.Background()
	store, err := mysql.Open(ctx, dsn, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	truncate(t, store)
	storagetest.RunMetadata(t, store)
}

func TestMySQLLongTokenDoesNotAuthenticateTruncatedPrefix(t *testing.T) {
	dsn := os.Getenv("OCTO_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set OCTO_TEST_MYSQL_DSN to run the MySQL contract test")
	}
	ctx := context.Background()
	store, err := mysql.Open(ctx, dsn, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	truncate(t, store)

	longToken := strings.Repeat("t", 300)
	truncated := longToken[:255]
	rec := storage.TokenRecord{Token: longToken, Created: "2026-07-13T17:15:00Z", Label: "long"}

	err = store.PutToken(ctx, longToken, rec)
	if err != nil {
		if got, getErr := store.GetToken(ctx, truncated); getErr != nil {
			t.Fatal(getErr)
		} else if got != nil {
			t.Fatalf("truncated token prefix authenticated after PutToken error")
		}
		return
	}

	got, err := store.GetToken(ctx, longToken)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Token != longToken {
		t.Fatalf("long token was not stored intact")
	}
	got, err = store.GetToken(ctx, truncated)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("truncated token prefix authenticated")
	}
}

func truncate(t *testing.T, store *mysql.Store) {
	t.Helper()
	if err := store.TruncateAll(context.Background()); err != nil {
		t.Fatal(err)
	}
}
