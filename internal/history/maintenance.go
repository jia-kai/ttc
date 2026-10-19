package history

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"ttc/internal/filelock"

	"golang.org/x/sys/unix"
)

const historyMaintenanceThreshold int64 = 2 * 1024 * 1024 * 1024
const historyReclaimTimeout = 2 * time.Minute

// MaintenanceResult reports the post-maintenance history footprint and the
// number of whole conversation families removed. Deletions may have committed
// even if later asset removal, disk reclamation, or statistics collection fails.
type MaintenanceResult struct {
	Stats           StorageStats
	RemovedLineages int
}

// MaintenanceProgress describes one sweep phase. Total zero means the amount
// of work is unknown. Counts describe processed candidates, not committed
// deletions; activity rechecks can retain candidates and SQL can still roll back.
type MaintenanceProgress struct {
	Phase            string
	Completed, Total int64
}

func reportMaintenance(report func(MaintenanceProgress), phase string, completed, total int64) {
	if report != nil {
		report(MaintenanceProgress{Phase: phase, Completed: completed, Total: total})
	}
}

// Maintain refreshes the loaded session's activity and, only when the initial
// database plus WAL lengths total at least 2 GiB, purges families inactive for
// thirty days. The protected session's entire family survives. It performs one
// sweep and never evicts newer families to reach a target size. Above the size
// threshold it also attempts to reclaim SQLite free pages and WAL residue,
// including space left by an earlier interrupted reclamation. Large databases
// with no deletions or free pages need only a checkpoint, not another VACUUM.
// Credentials, exports, and user workspace files are untouched. Reclamation has
// a two-minute limit, shortened by ctx's deadline.
//
// Calls serialize with this Store's writes. Other instances remain governed by
// SQLite locking and the transaction's activity recheck. VACUUM requires exclusive
// lifetime use and is deferred with an error while another Store is open; later
// maintenance can retry reclaiming the remaining free pages. Cancellation while
// waiting for this Store's mutex is checked once the current operation releases
// it. On error, RemovedLineages still counts committed deletions; Stats is valid
// only on success.
// The optional report callback runs synchronously under the Store mutex and
// must not call Store methods. No cleanup progress is emitted below the threshold.
func (s *Store) Maintain(ctx context.Context, now time.Time, protectedSession string, report func(MaintenanceProgress)) (MaintenanceResult, error) {
	return s.maintain(ctx, now, protectedSession, historyMaintenanceThreshold, report)
}

// RefreshLoadedActivity protects the loaded session's whole family from other
// instances' retention sweeps without scanning history statistics or purging.
// Empty or unsaved session IDs are harmless no-ops. Activity never moves backward.
func (s *Store) RefreshLoadedActivity(ctx context.Context, now time.Time, session string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.refreshLoadedActivity(ctx, now, session)
}

// maintain keeps the production threshold fixed while allowing small fixtures.
func (s *Store) maintain(ctx context.Context, now time.Time, protectedSession string, threshold int64, report func(MaintenanceProgress)) (MaintenanceResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result MaintenanceResult
	size, err := s.storageBytes(ctx)
	if err != nil {
		return result, fmt.Errorf("measure history before maintenance: %w", err)
	}
	if size >= threshold {
		result.RemovedLineages, err = s.cleanupLocked(ctx, now, protectedSession, report)
		if err != nil {
			err = fmt.Errorf("purge inactive history: %w", err)
		}
		if result.RemovedLineages > 0 {
			// Asset errors cannot undo committed SQL deletion. Still attempt disk
			// reclamation, and preserve both errors for the caller when it fails.
			err = errors.Join(err, s.reclaimHistory(ctx, true, report))
		} else if err == nil {
			var freePages int64
			if err = s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freePages); err != nil {
				err = fmt.Errorf("inspect reclaimable history pages: %w", err)
			} else {
				// Deletion may have committed in an earlier invocation whose
				// reclamation failed. SQLite's own free-page/WAL state is enough
				// to retry, without a repair journal or another forced eviction.
				err = s.reclaimHistory(ctx, freePages > 0, report)
			}
		}
		if err != nil {
			return result, err
		}
	} else if err := s.refreshLoadedActivity(ctx, now, protectedSession); err != nil {
		return result, err
	}
	if size >= threshold {
		reportMaintenance(report, "statistics", 0, 0)
	}
	result.Stats, err = s.storageStatsLocked(ctx)
	return result, err
}

func (s *Store) reclaimHistory(ctx context.Context, compact bool, report func(MaintenanceProgress)) error {
	ctx, cancel := context.WithTimeout(ctx, historyReclaimTimeout)
	defer cancel()
	if !compact {
		reportMaintenance(report, "checkpoint", 0, 0)
		return s.checkpointHistory(ctx)
	}
	reportMaintenance(report, "reclamation", 0, 0)
	return s.withExclusiveHistoryUse(ctx, func() error {
		// First release the sweep's potentially large WAL before allocating VACUUM's
		// compacted copy. Readers in other processes can prevent truncation, so fail
		// explicitly instead of repeatedly retrying or evicting newer history.
		reportMaintenance(report, "checkpoint", 0, 0)
		if err := s.checkpointHistory(ctx); err != nil {
			return err
		}
		// VACUUM preserves explicit INTEGER PRIMARY KEY identities and all logical
		// rows. In WAL mode its compacted pages must then be checkpointed to shrink
		// the physical files measured by StorageStats.
		reportMaintenance(report, "vacuum", 0, 0)
		if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
			return fmt.Errorf("compact history after purge: %w", err)
		}
		reportMaintenance(report, "checkpoint", 0, 0)
		return s.checkpointHistory(ctx)
	})
}

// withExclusiveHistoryUse requires s.mu and fences openers and other reclaimers
// with the initialization lock before converting this Store's lifetime lock.
func (s *Store) withExclusiveHistoryUse(ctx context.Context, run func() error) (err error) {
	init, err := filelock.Acquire(ctx, filepath.Join(s.Root, "history-init.lock"))
	if err != nil {
		return fmt.Errorf("coordinate history compaction: %w", err)
	}
	defer init.Close()
	if s.useLock == nil {
		return fmt.Errorf("compact history: store lifetime lock is closed")
	}
	fd := int(s.useLock.Fd())
	// flock conversion is not atomic and can drop the shared lock even on
	// failure. This is safe only while init fences openers and other reclaimers:
	// existing Stores never upgrade their lifetime lock without holding init.
	// Restore shared use before releasing init on every path, including a busy
	// conversion or canceled VACUUM. Restoration cannot contend under this fence.
	defer func() {
		var restoreErr error
		for {
			restoreErr = unix.Flock(fd, unix.LOCK_SH)
			if !errors.Is(restoreErr, unix.EINTR) {
				break
			}
		}
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore shared history lifetime lock: %w", restoreErr))
		}
	}()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("history compaction deferred: other TTC instances are using this data directory; close them and retry")
		}
		return fmt.Errorf("acquire exclusive history lifetime use for compaction: %w", err)
	}
	return run()
}

func (s *Store) checkpointHistory(ctx context.Context) error {
	var busy, frames, checkpointed int
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint history during reclamation: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint history during reclamation: database busy (%d of %d WAL frames checkpointed)", checkpointed, frames)
	}
	return nil
}
