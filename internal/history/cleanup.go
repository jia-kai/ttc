package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const lineageRetention = 30 * 24 * time.Hour

// Cleanup expires whole lineages inactive for at least thirty days. now is the
// sweep's wall-clock time; protectedSession may identify any predecessor or
// continuation, or an unsaved blank session. Its entire lineage and any durable
// filesystem journal's lineage are protected. Call after filesystem recovery,
// and serialize against session activation so its protection stays current.
//
// A private deletion marker precedes the SQL transaction. SQL rows are removed
// together; interrupted asset deletion resumes on the next sweep. Only owned
// private lineage trees are deleted, using directory descriptors without following
// symlinks. Credentials, model preferences and workspace generations survive.
// The result counts completed deletions, including resumed filesystem cleanup.
func (s *Store) Cleanup(ctx context.Context, now time.Time, protectedSession string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	protected := map[string]bool{}
	var lineage string
	if protectedSession != "" {
		err := s.DB.QueryRowContext(ctx, "SELECT lineage_id FROM sessions WHERE id=?", protectedSession).Scan(&lineage)
		if err == nil {
			protected[lineage] = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("protect loaded history: %w", err)
		}
	}
	err := s.DB.QueryRowContext(ctx, "SELECT s.lineage_id FROM fs_operation f JOIN sessions s ON s.id=f.session_id WHERE f.slot=1").Scan(&lineage)
	if err == nil {
		protected[lineage] = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("protect pending filesystem operation: %w", err)
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT lineage_id FROM sessions GROUP BY lineage_id HAVING max(last_activity_ms)<=? ORDER BY lineage_id", now.Add(-lineageRetention).UnixMilli())
	if err != nil {
		return 0, err
	}
	selected := map[string]bool{}
	for rows.Next() {
		if err = rows.Scan(&lineage); err != nil {
			rows.Close()
			return 0, err
		}
		selected[lineage] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	root, err := openCleanupDir(-1, s.Root, false)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	markers, err := openCleanupDir(int(root.Fd()), ".retention", true)
	if err != nil {
		return 0, err
	}
	defer markers.Close()
	assets, err := openCleanupDir(int(root.Fd()), "lineages", true)
	if err != nil {
		return 0, err
	}
	defer assets.Close()
	if err := root.Sync(); err != nil {
		return 0, fmt.Errorf("sync retention directories: %w", err)
	}
	for {
		entries, readErr := markers.ReadDir(128)
		for _, entry := range entries {
			selected[entry.Name()] = true
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, fmt.Errorf("read deletion markers: %w", readErr)
		}
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	completed := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return completed, err
		}
		if !cleanupID(id) {
			return completed, fmt.Errorf("invalid retention lineage ID %q", id)
		}
		if protected[id] {
			continue
		}
		// A marker may precede a rolled-back SQL deletion. If that still-live
		// lineage has since been used, retain it until it expires again. Missing
		// SQL rows instead mean asset cleanup must resume regardless of age.
		var activity sql.NullInt64
		if err := s.DB.QueryRowContext(ctx, "SELECT max(last_activity_ms) FROM sessions WHERE lineage_id=?", id).Scan(&activity); err != nil {
			return completed, err
		}
		if activity.Valid && activity.Int64 > now.Add(-lineageRetention).UnixMilli() {
			continue
		}
		if err := createCleanupMarker(markers, id); err != nil {
			return completed, fmt.Errorf("mark lineage %s for deletion: %w", id, err)
		}
		if err := s.deleteLineage(ctx, id); err != nil {
			return completed, fmt.Errorf("delete lineage %s history: %w", id, err)
		}
		if err := removeCleanupTree(ctx, assets, id); err != nil {
			return completed, fmt.Errorf("delete lineage %s assets; retry marker retained: %w", id, err)
		}
		if err := assets.Sync(); err != nil {
			return completed, err
		}
		if err := unix.Unlinkat(int(markers.Fd()), id, 0); err != nil {
			return completed, fmt.Errorf("remove lineage %s deletion marker: %w", id, err)
		}
		if err := markers.Sync(); err != nil {
			return completed, err
		}
		completed++
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

func createCleanupMarker(dir *os.File, id string) error {
	fd, err := unix.Openat(int(dir.Fd()), id, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err == unix.EEXIST {
		var info unix.Stat_t
		if err = unix.Fstatat(int(dir.Fd()), id, &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&0777 != 0600 || info.Uid != uint32(os.Geteuid()) || info.Size != 0 {
			return fmt.Errorf("deletion marker %s must be an owned empty 0600 regular file", id)
		}
		return nil
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), id)
	if err = f.Chmod(0600); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return dir.Sync()
}

func (s *Store) deleteLineage(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// All cross-table pointers belong to the lineage; defer cyclic entry/turn/
	// file/session checks until the complete lineage has been removed.
	if _, err = tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		return err
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
			return err
		}
	}
	return tx.Commit()
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
