package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
)

// advisoryLocker is a sluglock.Locker backed by PostgreSQL advisory locks, so
// per-slug serialization holds ACROSS app instances.
type advisoryLocker struct {
	pool *pgxpool.Pool
}

const advisoryUnlockTimeout = 2 * time.Second

var _ sluglock.Locker = (*advisoryLocker)(nil)

// advisoryKey maps an arbitrary lock key to a stable PostgreSQL int64 key.
func advisoryKey(key string) int64 {
	sum := sha256.Sum256([]byte(key))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// With runs fn while holding the slug's advisory lock, releasing it afterward.
func (l *advisoryLocker) With(ctx context.Context, key string, fn func() error) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := advisoryKey(key)
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire lock conn: %w", err)
	}
	release := true
	defer func() {
		if release {
			conn.Release()
		}
	}()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", id); err != nil {
		return fmt.Errorf("pg_advisory_lock: %w", err)
	}
	// Unlock on the same connection with an independent bounded context. On any
	// failure, hijack and close the physical connection so a locked session can
	// never return to the pool.
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), advisoryUnlockTimeout)
		defer cancel()
		var unlocked bool
		unlockErr := conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", id).Scan(&unlocked)
		if unlockErr == nil && unlocked {
			return
		}
		if unlockErr == nil {
			unlockErr = fmt.Errorf("pg_advisory_unlock returned false")
		}
		release = false
		physical := conn.Hijack()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), advisoryUnlockTimeout)
		defer closeCancel()
		closeErr := physical.Close(closeCtx)
		if retErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("pg_advisory_unlock: %w", unlockErr), closeErr)
			return
		}
		// The critical section already committed durably: surfacing an unlock
		// failure as a publish error would invite a retry that re-runs the
		// locked section and mints a duplicate version (the exact failure mode
		// Promote refuses to create). The session is destroyed anyway, so the
		// lock dies with the connection.
		slog.Default().Warn("pg_advisory_unlock failed after commit; connection destroyed", "key", key, "err", unlockErr, "close_err", closeErr)
	}()

	return fn()
}
