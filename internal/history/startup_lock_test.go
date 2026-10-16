package history

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"ttc/internal/filelock"
)

func TestStoresShareLifetimeLockAndExcludeMigration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	path := filepath.Join(root, "history-use.lock")
	assertBlocked := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		f, err := filelock.Acquire(ctx, path)
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("upgrade lock with active store: %v", err)
		}
	}
	assertBlocked()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	assertBlocked()
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f, err := filelock.Acquire(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestCurrentStartupDoesNotAcquireDatabaseWriterLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	tx, err := first.db.Begin() // _txlock=immediate holds the SQLite writer lock.
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	second, err := Open(root)
	if err != nil {
		t.Fatalf("current startup blocked behind a normal commit: %v", err)
	}
	second.Close()
}
