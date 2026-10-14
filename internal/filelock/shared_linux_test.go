package filelock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSharedLocksExcludeUpgradeUntilAllReadersClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "use.lock")
	first, err := AcquireShared(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := AcquireShared(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if f, err := Acquire(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("exclusive lock with readers: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if f, err := Acquire(ctx2, path); !errors.Is(err, context.DeadlineExceeded) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("exclusive lock with remaining reader: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	exclusive, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer exclusive.Close()
	ctx3, cancel3 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel3()
	if f, err := AcquireShared(ctx3, path); !errors.Is(err, context.DeadlineExceeded) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("shared lock during upgrade: %v", err)
	}
}
