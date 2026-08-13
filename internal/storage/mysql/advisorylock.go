package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
)

type advisoryLocker struct {
	db *sql.DB
}

const advisoryUnlockTimeout = 2 * time.Second

var _ sluglock.Locker = (*advisoryLocker)(nil)

func advisoryName(key string) string {
	// Hash rather than truncate the user key: MySQL named locks are bounded and
	// a collision only serializes unrelated work, never aliases document data.
	sum := sha256.Sum256([]byte(key))
	return "octodoc:" + hex.EncodeToString(sum[:])[:56]
}

// With runs fn while holding a MySQL named lock. GET_LOCK is connection-scoped,
// so acquire, fn, and release are bound to the same dedicated *sql.Conn.
// ctx controls acquisition and fn; release uses a fresh bounded context so a
// canceled caller cannot leave a lock held by a pooled connection.
func (l *advisoryLocker) With(ctx context.Context, key string, fn func() error) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire lock conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	name := advisoryName(key)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var got sql.NullInt64
		if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 1)", name).Scan(&got); err != nil {
			return fmt.Errorf("get_lock: %w", err)
		}
		if !got.Valid {
			return fmt.Errorf("get_lock returned NULL")
		}
		if got.Int64 == 1 {
			break
		}
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), advisoryUnlockTimeout)
		defer cancel()
		var unlocked sql.NullInt64
		unlockErr := conn.QueryRowContext(unlockCtx, "SELECT RELEASE_LOCK(?)", name).Scan(&unlocked)
		if unlockErr == nil && unlocked.Valid && unlocked.Int64 == 1 {
			return
		}
		if unlockErr == nil {
			unlockErr = fmt.Errorf("release_lock returned %v", unlocked)
		}
		// database/sql has no public Conn.Hijack. ErrBadConn from Raw tells the
		// pool to destroy the underlying connection rather than reuse the session.
		discardErr := conn.Raw(func(any) error { return driver.ErrBadConn })
		if errors.Is(discardErr, driver.ErrBadConn) {
			discardErr = nil
		}
		if retErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release_lock: %w", unlockErr), discardErr)
			return
		}
		// The critical section already committed durably: an unlock failure here
		// must not masquerade as a publish error and invite a retry that mints a
		// duplicate version. The session is discarded, so the lock dies with it.
		slog.Default().Warn("release_lock failed after commit; connection discarded", "name", name, "err", unlockErr, "discard_err", discardErr)
	}()

	return fn()
}
