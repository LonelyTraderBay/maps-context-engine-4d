package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	moderncsqlite "modernc.org/sqlite"
)

var ErrBusyTimeout = errors.New("SQLite busy timeout exceeded")

// writeTx is private until T010 wires writers through lifecycle and quota admission.
// It uses short driver busy waits so context deadlines can stop lock retries.
type writeTx struct {
	conn        *sql.Conn
	busyTimeout time.Duration
}

// writeRow defers a single-row read until Scan so SQLITE_BUSY can be retried
// without exposing an unbounded driver busy wait.
type writeRow struct {
	tx    *writeTx
	ctx   context.Context
	query string
	args  []any
}

// withWriteTx serializes writes through the OS writer lease and one-connection
// pool, admits the bounded peak before touching SQLite, and applies a
// context-aware wait bounded by the effective SQLite busy timeout.
func (store *Store) withWriteTx(ctx context.Context, peakGrowthBytes int64, write func(*writeTx) error) (retErr error) {
	if !validWriteRequest(store, ctx, peakGrowthBytes, write) {
		return fmt.Errorf("invalid SQLite write transaction request: %w", ErrInvalidIndex)
	}
	if token, ok := ctx.Value(generationWriteContextKey{}).(*generationWriteToken); ok && token.store == store {
		if done, active := token.enter(); active {
			defer done()
			return store.withWriteTxUnderLease(ctx, peakGrowthBytes, write)
		}
	}
	store.writeGate.RLock()
	defer store.writeGate.RUnlock()
	if store.closed {
		return fmt.Errorf("SQLite store is closed: %w", ErrInvalidIndex)
	}
	writerLease, err := store.ownership.acquireWriter(ctx)
	if err != nil {
		return fmt.Errorf("acquire worktree writer lease: %w", err)
	}
	defer func() {
		if err := writerLease.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release worktree writer lease: %w", err))
		}
	}()
	return store.withWriteTxUnderLease(ctx, peakGrowthBytes, write)
}

func validWriteRequest(store *Store, ctx context.Context, peakGrowthBytes int64, write func(*writeTx) error) bool {
	return store != nil && store.writer != nil && store.ownership != nil && ctx != nil && write != nil &&
		store.busyTimeoutMS >= 1 && peakGrowthBytes >= 1 && peakGrowthBytes <= model.MaxSafeInteger
}

// withWriteTxUnderLease starts a bounded SQLite transaction while the caller
// already owns the worktree OS writer lease and Store write gate.
func (store *Store) withWriteTxUnderLease(ctx context.Context, peakGrowthBytes int64, write func(*writeTx) error) (retErr error) {
	if !validWriteRequest(store, ctx, peakGrowthBytes, write) {
		return fmt.Errorf("invalid leased SQLite write transaction request: %w", ErrInvalidIndex)
	}
	if store.closed {
		return fmt.Errorf("SQLite store is closed: %w", ErrInvalidIndex)
	}
	reservation, err := store.ownership.admitIndexGrowth(ctx, peakGrowthBytes)
	if err != nil {
		return fmt.Errorf("admit SQLite write growth: %w", err)
	}
	defer func() {
		if err := reservation.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("settle SQLite write admission: %w", err))
		}
	}()
	conn, err := store.writer.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire SQLite write connection: %w", err)
	}
	changedTimeout := false
	inTransaction := false
	discard := false
	defer func() {
		if inTransaction {
			if _, rollbackErr := conn.ExecContext(context.Background(), `ROLLBACK`); rollbackErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("rollback SQLite write transaction: %w", rollbackErr))
				discard = true
			}
		}
		if changedTimeout && !discard {
			if _, restoreErr := conn.ExecContext(context.Background(), fmt.Sprintf(`PRAGMA busy_timeout=%d`, store.busyTimeoutMS)); restoreErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("restore SQLite busy timeout: %w", restoreErr))
				discard = true
			}
		}
		if discard {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		if closeErr := conn.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release SQLite write connection: %w", closeErr))
		}
	}()

	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		discard = true
		return fmt.Errorf("set cancellable SQLite busy wait: %w", err)
	}
	changedTimeout = true
	if err := retryBusy(ctx, time.Duration(store.busyTimeoutMS)*time.Millisecond, func() error {
		_, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`)
		return err
	}); err != nil {
		return fmt.Errorf("begin SQLite write transaction: %w", err)
	}
	inTransaction = true

	transaction := &writeTx{conn: conn, busyTimeout: time.Duration(store.busyTimeoutMS) * time.Millisecond}
	if err := write(transaction); err != nil {
		return err
	}
	if err := retryBusy(ctx, transaction.busyTimeout, func() error {
		_, err := conn.ExecContext(ctx, `COMMIT`)
		return err
	}); err != nil {
		return fmt.Errorf("commit SQLite write transaction: %w", err)
	}
	inTransaction = false
	return nil
}

// ExecContext executes a statement, retrying only SQLite BUSY/LOCKED results.
func (tx *writeTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if tx == nil || tx.conn == nil || ctx == nil {
		return nil, fmt.Errorf("invalid SQLite write statement: %w", ErrInvalidIndex)
	}
	var result sql.Result
	err := retryBusy(ctx, tx.busyTimeout, func() error {
		var execErr error
		result, execErr = tx.conn.ExecContext(ctx, query, args...)
		return execErr
	})
	return result, err
}

// QueryRowContext returns a row whose Scan performs context-aware lock retry.
func (tx *writeTx) QueryRowContext(ctx context.Context, query string, args ...any) *writeRow {
	return &writeRow{tx: tx, ctx: ctx, query: query, args: append([]any(nil), args...)}
}

// Scan executes and scans a single row, retrying when the database is briefly
// locked and returning the context error as soon as its deadline is reached.
func (row *writeRow) Scan(dest ...any) error {
	if row == nil || row.tx == nil || row.tx.conn == nil || row.ctx == nil {
		return fmt.Errorf("invalid SQLite write query: %w", ErrInvalidIndex)
	}
	return retryBusy(row.ctx, row.tx.busyTimeout, func() error {
		rows, err := row.tx.conn.QueryContext(row.ctx, row.query, row.args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return err
			}
			return sql.ErrNoRows
		}
		return rows.Scan(dest...)
	})
}

func retryBusy(ctx context.Context, timeout time.Duration, attempt func() error) error {
	if ctx == nil {
		return fmt.Errorf("nil context: %w", ErrInvalidIndex)
	}
	if timeout <= 0 {
		return fmt.Errorf("nonpositive SQLite busy timeout: %w", ErrInvalidIndex)
	}
	deadline := time.Now().Add(timeout)
	delay := time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := attempt()
		if err == nil || !isSQLiteBusy(err) {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("%w: %w", ErrBusyTimeout, err)
		}
		wait := delay
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < 25*time.Millisecond {
			delay *= 2
			if delay > 25*time.Millisecond {
				delay = 25 * time.Millisecond
			}
		}
	}
}

func isSQLiteBusy(err error) bool {
	var sqliteErr *moderncsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	primaryCode := sqliteErr.Code() & 0xff
	return primaryCode == 5 || primaryCode == 6 // SQLITE_BUSY or SQLITE_LOCKED
}
