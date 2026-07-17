package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
)

// advisoryLocker is a sluglock.Locker backed by PostgreSQL advisory locks, so
// per-slug serialization holds ACROSS app instances (the in-process
// sluglock.Memory only serializes within one process). Publishing the same slug
// from two instances can otherwise resolve to the same version and clobber each
// other's blob/meta; the advisory lock closes that window.
//
// Advisory locks are session-scoped: pg_advisory_lock(key) is held by the
// connection until pg_advisory_unlock or the session ends. So each With acquires
// a DEDICATED pooled connection for the lock alone — separate from whatever
// connection fn uses for its own queries — locks, runs fn, then unlocks and
// releases in a defer. The pool must therefore allow at least 2 connections
// (PG_POOL_MAX default 10).
type advisoryLocker struct {
	pool *pgxpool.Pool
}

var _ sluglock.Locker = (*advisoryLocker)(nil)

// advisoryKey maps an arbitrary lock key (slug or a sentinel) to the int64 that
// pg_advisory_lock takes. sha256's first 8 bytes give a stable, well-distributed
// value. A collision only makes two unrelated keys occasionally serialize against
// each other — a negligible perf cost, never a correctness problem.
func advisoryKey(key string) int64 {
	sum := sha256.Sum256([]byte(key))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// With runs fn while holding the slug's advisory lock, releasing it afterward.
// The context is honored before acquiring and while waiting for the lock, so a
// cancelled request doesn't block forever.
func (l *advisoryLocker) With(ctx context.Context, key string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := advisoryKey(key)
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire lock conn: %w", err)
	}
	defer conn.Release()

	// Blocks until granted (matching Memory.With's semantics); ctx cancellation
	// aborts the wait.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", id); err != nil {
		return fmt.Errorf("pg_advisory_lock: %w", err)
	}
	// Unlock on the SAME connection that acquired it (advisory locks are
	// per-session). Use a background context so unlock still runs even if the
	// request ctx was cancelled during fn.
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", id)
	}()

	return fn()
}
