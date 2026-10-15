package history

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/filelock"

	"golang.org/x/sys/unix"
)

func assertLifetimeShared(t *testing.T, s *Store) {
	t.Helper()
	probe, err := os.OpenFile(filepath.Join(s.Root, "history-use.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatal("Store lost its lifetime lock", err)
	}
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal("Store retained exclusive rather than shared lifetime use", err)
	}
}

func TestMaintainDefersCompactionUntilOtherStoreCloses(t *testing.T) {
	s, old, _, _ := historyFixture(t)
	if _, err := s.DB.Exec("UPDATE entries SET content_json=json_set(content_json,'$.content',?)", strings.Repeat("old payload", 100000)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ageLineage(t, s, old.LineageID, now.Add(-31*24*time.Hour))
	other, err := Open(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	result, err := s.maintain(context.Background(), now, "", 1)
	if err == nil || !strings.Contains(err.Error(), "compaction deferred") || result.RemovedLineages != 1 {
		t.Fatal("expected purge with deferred compaction", result, err)
	}
	beforeRetry := physicalHistoryBytes(t, s)
	// Deferral must leave other Stores able to commit normally, without a
	// long-running VACUUM holding the SQLite writer lock.
	active, _ := startHistorySession(t, other, t.TempDir(), old.Model, "new recent work")
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	// With the other lifetime reader gone, this detects a shared lock lost by
	// the failed SH->EX conversion, before a new opener can hide that mistake.
	assertLifetimeShared(t, s)
	opener, err := Open(s.Root)
	if err != nil {
		t.Fatal("failed conversion blocked an ordinary opener", err)
	}
	opener.Close()
	result, err = s.maintain(context.Background(), now, active.ID, 1)
	if err != nil || result.RemovedLineages != 0 || result.Stats.Bytes >= beforeRetry/2 || result.Stats.Entries == 0 {
		t.Fatal("deferred compaction did not retry without further eviction", beforeRetry, result, err)
	}
	assertLifetimeShared(t, s)
}

func TestExclusiveHistoryUseRestoresLifetimeLockAfterError(t *testing.T) {
	s, _, _, _ := historyFixture(t)
	want := errors.New("controlled compaction failure")
	s.mu.Lock()
	err := s.withExclusiveHistoryUse(context.Background(), func() error {
		probe, err := os.OpenFile(filepath.Join(s.Root, "history-use.lock"), os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer probe.Close()
		if err := unix.Flock(int(probe.Fd()), unix.LOCK_SH|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
			t.Error("compaction did not hold exclusive lifetime use", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		init, err := filelock.Acquire(ctx, filepath.Join(s.Root, "history-init.lock"))
		if init != nil {
			init.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Error("compaction did not fence openers and other reclaimers", err)
		}
		return want
	})
	s.mu.Unlock()
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
	assertLifetimeShared(t, s)
	opener, err := Open(s.Root)
	if err != nil {
		t.Fatal("failed compaction did not release init/exclusive-use locks", err)
	}
	opener.Close()
}

func TestCompactionInitLockWaitHonorsContext(t *testing.T) {
	s, _, _, _ := historyFixture(t)
	init, err := filelock.Acquire(context.Background(), filepath.Join(s.Root, "history-init.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer init.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	s.mu.Lock()
	err = s.reclaimHistory(ctx, true)
	s.mu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("compaction ignored initialization-lock wait deadline", err)
	}
	assertLifetimeShared(t, s)
}

func TestCurrentOpenLifetimeWaitReportsMaintenance(t *testing.T) {
	s, _, _, _ := historyFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := filelock.Acquire(context.Background(), filepath.Join(s.Root, "history-use.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	opened, err := Open(s.Root)
	if opened != nil {
		opened.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "history maintenance in progress; retry when finished") || strings.Contains(err.Error(), "stop all TTC instances") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("current-schema opener gave misleading migration guidance", err)
	}
}
