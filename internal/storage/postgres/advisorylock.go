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

const advisoryUnlockTimeout = 2 * time.Second

var (
	_              sluglock.Locker        = (*advisoryLocker)(nil)
	_              sluglock.SessionLocker = (*advisoryLocker)(nil)
	advisoryLogger                        = slog.Default()
)

// SetAdvisoryLockLogger injects the logger used for advisory-lock recovery.
func SetAdvisoryLockLogger(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	advisoryLogger = logger
}

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
// cancelled request doesn't block forever. ctx controls fn; unlock deliberately
// uses a fresh bounded context so cancellation cannot strand a session-scoped lock.
func (l *advisoryLocker) With(ctx context.Context, key string, fn func() error) (retErr error) {
	return l.Session(ctx, func(session sluglock.LockSession) error {
		if err := session.Acquire(ctx, key); err != nil {
			return err
		}
		return fn()
	})
}

type advisorySession struct {
	conn *pgxpool.Conn
	keys []string
}

func (s *advisorySession) Acquire(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryKey(key)); err != nil {
		return fmt.Errorf("pg_advisory_lock: %w", err)
	}
	s.keys = append(s.keys, key)
	return nil
}

// Session keeps every advisory lock on one pooled connection.
func (l *advisoryLocker) Session(ctx context.Context, fn func(sluglock.LockSession) error) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
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

	session := &advisorySession{conn: conn}
	// Unlock on the same connection with an independent bounded context. On an
	// unlock failure, hijack and close the physical connection so the locked
	// session can never return to the pool.
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), advisoryUnlockTimeout)
		defer cancel()
		var unlockErr error
		for i := len(session.keys) - 1; i >= 0; i-- {
			var unlocked bool
			err := conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", advisoryKey(session.keys[i])).Scan(&unlocked)
			if err == nil && unlocked {
				continue
			}
			if err == nil {
				err = fmt.Errorf("pg_advisory_unlock returned false")
			}
			unlockErr = errors.Join(unlockErr, err)
		}
		if unlockErr == nil {
			return
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
		advisoryLogger.Warn("pg_advisory_unlock failed after commit; connection destroyed", "keys", session.keys, "err", unlockErr, "close_err", closeErr)
	}()
	return fn(session)
}
