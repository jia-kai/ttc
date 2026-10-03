package history

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const lineageRetention = 30 * 24 * time.Hour

// Cleanup expires entire lineages inactive for thirty days, protecting the loaded
// conversation and its copies. SQL deletion is atomic and rechecks activity under
// the database write transaction. Asset deletion is best effort; there are no
// deletion journals or restart repairs. The result counts deleted lineages.
func (s *Store) Cleanup(ctx context.Context, now time.Time, protectedSession string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Refresh the loaded lineage so other instances' sweeps also retain it.
	if _, err := s.DB.ExecContext(ctx, "UPDATE sessions SET last_activity_ms=? WHERE id=?", now.UnixMilli(), protectedSession); err != nil {
		return 0, err
	}
	cutoff := now.Add(-lineageRetention).UnixMilli()
	rows, err := s.DB.QueryContext(ctx, "SELECT lineage_id FROM sessions GROUP BY lineage_id HAVING max(last_activity_ms)<=? ORDER BY lineage_id", cutoff)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	assets, err := openCleanupDir(-1, filepath.Join(s.Root, "lineages"), true)
	if err != nil {
		return 0, err
	}
	defer assets.Close()
	completed := 0
	for _, id := range ids {
		if !cleanupID(id) {
			return completed, fmt.Errorf("invalid retention lineage ID %q", id)
		}
		deleted, err := s.deleteLineage(ctx, id, cutoff)
		if err != nil {
			return completed, fmt.Errorf("delete lineage %s history: %w", id, err)
		}
		if !deleted {
			continue
		}
		completed++
		if err := removeCleanupTree(ctx, assets, id); err != nil {
			return completed, fmt.Errorf("delete lineage %s assets: %w", id, err)
		}
	}
	return completed, nil
}

func cleanupID(id string) bool {
	return id != "" && len(id) <= 128 && strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") == ""
}

// openCleanupDir pins an owned private directory and rejects final symlinks.
func openCleanupDir(parent int, name string, create bool) (*os.File, error) {
	if create {
		if err := unix.Mkdirat(parent, name, 0700); err != nil && err != unix.EEXIST {
			return nil, err
		}
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open private retention directory %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	var info unix.Stat_t
	if err = unix.Fstat(fd, &info); err != nil {
		f.Close()
		return nil, err
	}
	if info.Uid != uint32(os.Geteuid()) || info.Mode&0777 != 0700 {
		f.Close()
		return nil, fmt.Errorf("retention directory %s must be owned by UID %d with mode 0700", name, os.Geteuid())
	}
	return f, nil
}

func (s *Store) deleteLineage(ctx context.Context, id string, cutoff int64) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var activity sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT max(last_activity_ms) FROM sessions WHERE lineage_id=?", id).Scan(&activity); err != nil {
		return false, err
	}
	if !activity.Valid || activity.Int64 > cutoff {
		return false, nil
	}
	// All cross-table pointers belong to the lineage; defer cyclic entry/turn/
	// file/session checks until the complete lineage has been removed.
	if _, err = tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		return false, err
	}
	const sessions = "SELECT id FROM sessions WHERE lineage_id=?"
	queries := []string{
		"DELETE FROM compactions WHERE continuation_id IN (" + sessions + ")",
		"DELETE FROM tool_records WHERE entry_id IN (SELECT id FROM entries WHERE session_id IN (" + sessions + "))",
		"DELETE FROM file_changes WHERE session_id IN (" + sessions + ")",
		"DELETE FROM tool_calls WHERE session_id IN (" + sessions + ")",
		"DELETE FROM model_requests WHERE session_id IN (" + sessions + ")",
		"DELETE FROM entries WHERE session_id IN (" + sessions + ")",
		"DELETE FROM turns WHERE session_id IN (" + sessions + ")",
		"DELETE FROM sessions WHERE lineage_id=?",
	}
	for _, query := range queries {
		if _, err = tx.ExecContext(ctx, query, id); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func removeCleanupTree(ctx context.Context, parent *os.File, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var info unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), name, &info, unix.AT_SYMLINK_NOFOLLOW)
	if err == unix.ENOENT {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("retention asset %s is owned by another UID", name)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFDIR {
		// Unlink a symlink itself, never inspect or delete its target.
		return unix.Unlinkat(int(parent.Fd()), name, 0)
	}
	child, err := openCleanupDir(int(parent.Fd()), name, false)
	if err != nil {
		return err
	}
	defer child.Close()
	for {
		entries, readErr := child.ReadDir(128)
		for _, entry := range entries {
			if err = removeCleanupTree(ctx, child, entry.Name()); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}
