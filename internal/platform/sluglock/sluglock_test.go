package sluglock

import (
	"context"
	"strconv"
	"sync"
	"testing"
)

func TestSerializesPerKey(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	const n = 100
	counter := 0
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			_ = m.With(ctx, "same-key", func() error {
				// Non-atomic increment; correct only if calls are serialized.
				counter++
				return nil
			})
		})
	}
	wg.Wait()
	if counter != n {
		t.Fatalf("counter = %d, want %d (lock did not serialize)", counter, n)
	}
}

func TestDifferentKeysIndependent(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	got := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, k := range []string{"a", "b", "c"} {
		wg.Go(func() {
			_ = m.With(ctx, k, func() error {
				mu.Lock()
				got[k] = true
				mu.Unlock()
				return nil
			})
		})
	}
	wg.Wait()
	if len(got) != 3 {
		t.Fatalf("expected all 3 keys to run, got %d", len(got))
	}
}

func TestContextCancellationHonored(t *testing.T) {
	m := NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := m.With(ctx, "k", func() error { return nil })
	if err == nil {
		t.Error("cancelled context should be honored before acquiring")
	}
}

func TestMapReclaimedAfterUse(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	const n = 1000
	var wg sync.WaitGroup
	for i := range n {
		key := "slug-" + string(rune('a'+i%26)) + strconv.Itoa(i)
		wg.Go(func() {
			_ = m.With(ctx, key, func() error { return nil })
		})
	}
	wg.Wait()
	m.mu.Lock()
	remaining := len(m.locks)
	m.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("lock map not reclaimed: %d entries remain (want 0)", remaining)
	}
}
