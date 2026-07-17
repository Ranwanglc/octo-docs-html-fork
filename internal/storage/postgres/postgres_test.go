package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/postgres"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/storagetest"
)

// TestPostgresContract runs the storage contract against a real PostgreSQL when
// OCTO_TEST_DATABASE_URL is set; otherwise it is skipped.
func TestPostgresContract(t *testing.T) {
	url := os.Getenv("OCTO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OCTO_TEST_DATABASE_URL to run the PostgreSQL contract test")
	}
	ctx := context.Background()
	store, err := postgres.Open(ctx, url, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	truncate(t, store)
	storagetest.RunMetadata(t, store)
}

// truncate clears all tables so the contract starts from an empty store.
func truncate(t *testing.T, store *postgres.Store) {
	t.Helper()
	if err := store.TruncateAll(context.Background()); err != nil {
		t.Fatal(err)
	}
}
