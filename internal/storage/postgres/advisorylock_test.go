package postgres_test

import (
	"context"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/lml2468/octo-doc/internal/storage/postgres"
)

// TestAdvisoryLockerSerializes verifies the PostgreSQL advisory locker actually
// serializes concurrent work on the same key across connections — the property
// that makes multi-instance publishing safe. Gated on OCTO_TEST_DATABASE_URL.
func TestAdvisoryLockerSerializes(t *testing.T) {
	url := os.Getenv("OCTO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OCTO_TEST_DATABASE_URL to run the advisory-lock test")
	}
	ctx := context.Background()
	store, err := postgres.Open(ctx, url, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	locker := store.Locker()

	// Same key: a non-atomic read-modify-write of a shared counter is only correct
	// if the lock serializes the goroutines. Enough iterations that an unlocked
	// version would almost certainly lose an update.
	const n = 50
	counter := 0
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			_ = locker.With(ctx, "same-slug", func() error {
				v := counter
				runtime.Gosched() // widen the race window inside the critical section
				counter = v + 1
				return nil
			})
		})
	}
	wg.Wait()
	if counter != n {
		t.Fatalf("advisory lock did not serialize same-key work: counter=%d, want %d", counter, n)
	}

	// Distinct keys must NOT serialize: holding key-a then acquiring key-b nested
	// requires two locks held at once. If distinct keys collided, this would block;
	// a completion check within a timeout proves independence.
	done := make(chan struct{})
	go func() {
		_ = locker.With(ctx, "key-a", func() error {
			return locker.With(ctx, "key-b", func() error { return nil })
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("distinct keys serialized (nested distinct locks did not complete)")
	}
}
