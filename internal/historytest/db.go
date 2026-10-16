// Package historytest provides independent SQLite connections for storage
// assertions and fault injection in tests. Production code must use history's
// typed operations instead. Opening by Root intentionally does not expose or
// replace a Store's private connection pool or lifetime locks.
package historytest

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"sync"
	"testing"

	"ttc/internal/history"
)

type connectionKey struct {
	t     testing.TB
	store *history.Store
}

var connections = struct {
	sync.Mutex
	db map[connectionKey]*sql.DB
}{db: make(map[connectionKey]*sql.DB)}

// DB opens the existing history.sqlite under store.Root, without creating or
// migrating it. One independent connection pool is reused per test and Store;
// the test owns its lifetime through automatic cleanup. It uses immediate write
// transactions, foreign keys and the same five-second busy timeout as history.
func DB(t testing.TB, store *history.Store) *sql.DB {
	t.Helper()
	key := connectionKey{t, store}
	connections.Lock()
	defer connections.Unlock()
	if db := connections.db[key]; db != nil {
		return db
	}
	uri := url.URL{Scheme: "file", Path: filepath.Join(store.Root, "history.sqlite"), RawQuery: "mode=rw&_txlock=immediate"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, query := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON", "PRAGMA synchronous=FULL"} {
		if _, err := db.Exec(query); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	connections.db[key] = db
	t.Cleanup(func() {
		connections.Lock()
		delete(connections.db, key)
		connections.Unlock()
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

// WriterLock holds SQLite's writer reservation until Close, blocking production
// writes without reaching into the Store connection pool. Close is idempotent.
type WriterLock struct {
	tx   *sql.Tx
	once sync.Once
	err  error
}

// LockWriter begins an immediate transaction for deterministic cancellation and
// contention tests. Callers must close it before waiting for a blocked writer.
func LockWriter(t testing.TB, store *history.Store, ctx context.Context) (*WriterLock, error) {
	t.Helper()
	tx, err := DB(t, store).BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	lock := &WriterLock{tx: tx}
	t.Cleanup(func() { _ = lock.Close() })
	return lock, nil
}

// Close releases the writer reservation, rolling back any test changes.
func (lock *WriterLock) Close() error {
	lock.once.Do(func() {
		lock.err = lock.tx.Rollback()
		if errors.Is(lock.err, sql.ErrTxDone) {
			lock.err = nil
		}
	})
	return lock.err
}
