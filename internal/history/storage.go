package history

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

// StorageStats describes canonical history storage, excluding assets, exports,
// credentials, and SQLite shared-memory files. Other processes may change the
// database between collecting file lengths and entry statistics.
type StorageStats struct {
	Bytes    int64 // Physical file lengths of history.sqlite and history.sqlite-wal.
	Entries  int64 // All canonical entry rows, including invisible/status entries.
	OldestMS int64 // Minimum entry created_ms (Unix milliseconds); zero when empty.
}

// StorageStats returns physical database/WAL lengths and canonical entry counts.
// An epoch timestamp and empty history both have OldestMS zero; Entries
// distinguishes them. This call serializes with writes from this Store only.
func (s *Store) StorageStats(ctx context.Context) (StorageStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storageStatsLocked(ctx)
}

func (s *Store) storageStatsLocked(ctx context.Context) (StorageStats, error) {
	var stats StorageStats
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT count(*),min(created_ms) FROM entries").Scan(&stats.Entries, &oldest); err != nil {
		return stats, fmt.Errorf("read history entry statistics: %w", err)
	}
	if oldest.Valid {
		stats.OldestMS = oldest.Int64
	}
	var err error
	stats.Bytes, err = s.storageBytes(ctx)
	return stats, err
}

func (s *Store) storageBytes(ctx context.Context) (int64, error) {
	var size int64
	for _, suffix := range []string{"", "-wal"} {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		info, err := os.Lstat(filepath.Join(s.Root, "history.sqlite") + suffix)
		if os.IsNotExist(err) && suffix == "-wal" {
			continue // A checkpoint or another connection can remove the WAL.
		}
		if err != nil {
			return 0, fmt.Errorf("stat history.sqlite%s: %w", suffix, err)
		}
		if !info.Mode().IsRegular() {
			return 0, fmt.Errorf("history.sqlite%s must be a regular file", suffix)
		}
		size += info.Size()
	}
	return size, nil
}
