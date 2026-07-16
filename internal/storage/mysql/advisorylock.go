package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/lml2468/octo-doc/internal/platform/sluglock"
)

type advisoryLocker struct {
	db *sql.DB
}

var _ sluglock.Locker = (*advisoryLocker)(nil)

func advisoryName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "octodoc:" + hex.EncodeToString(sum[:])[:56]
}

// With runs fn while holding a MySQL named lock. GET_LOCK is connection-scoped,
// so acquire, fn, and release are bound to the same dedicated *sql.Conn.
func (l *advisoryLocker) With(ctx context.Context, key string, fn func() error) error {
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
		_, _ = conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", name)
	}()

	return fn()
}
