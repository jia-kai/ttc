package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const lineageRetention = 30 * 24 * time.Hour

// cleanupLocked expires whole inactive lineages and requires s.mu. The loaded
// conversation protects its whole family. SQL deletion is atomic and rechecks
// activity under the write transaction; asset deletion is best effort, without
// deletion journals or restart repair. The result counts committed deletions.
func (s *Store) cleanupLocked(ctx context.Context, now time.Time, protectedSession string, report func(MaintenanceProgress)) (result MaintenanceResult, err error) {
	reportMaintenance(report, "discovery", 0, 0)
	if err := ctx.Err(); err != nil {
		return MaintenanceResult{}, err
	}
	// Refresh the loaded lineage so other instances' sweeps also retain it.
	if err := s.refreshLoadedActivity(ctx, now, protectedSession); err != nil {
		return MaintenanceResult{}, err
	}
	cutoff := now.Add(-lineageRetention).UnixMilli()
	rows, err := s.db.QueryContext(ctx, "SELECT lineage_id FROM sessions GROUP BY lineage_id HAVING max(last_activity_ms)<=? ORDER BY lineage_id", cutoff)
	if err != nil {
		return MaintenanceResult{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return MaintenanceResult{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return MaintenanceResult{}, err
	}
	if len(ids) == 0 {
		return MaintenanceResult{}, nil
	}
	for _, id := range ids {
		if !cleanupID(id) {
			return MaintenanceResult{}, fmt.Errorf("invalid retention lineage ID %q", id)
		}
	}
	assets, err := openCleanupDir(-1, filepath.Join(s.Root, "lineages"), true)
	if err != nil {
		return MaintenanceResult{}, err
	}
	defer assets.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MaintenanceResult{}, err
	}
	defer tx.Rollback()
	// A sweep builds child-key indexes only once, and removes them in this same
	// transaction. A crash or failed sweep cannot leave schema additions behind.
	if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		return MaintenanceResult{}, err
	}
	var deletedIDs []string
	var deletedEntries int64
	reportMaintenance(report, "candidates", 0, int64(len(ids)))
	for i, id := range ids {
		var activity sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT max(last_activity_ms) FROM sessions WHERE lineage_id=?", id).Scan(&activity); err != nil {
			return MaintenanceResult{}, err
		}
		if !activity.Valid || activity.Int64 > cutoff {
			reportMaintenance(report, "candidates", int64(i+1), int64(len(ids)))
			continue
		}
		if len(deletedIDs) == 0 {
			reportMaintenance(report, "indexes", 0, 0)
			if err := indexCleanupReferences(ctx, tx); err != nil {
				return MaintenanceResult{}, err
			}
			reportMaintenance(report, "candidates", int64(i), int64(len(ids)))
		}
		entries, err := deleteLineage(ctx, tx, id)
		if err != nil {
			return MaintenanceResult{}, fmt.Errorf("delete lineage %s history: %w", id, err)
		}
		deletedEntries += entries
		deletedIDs = append(deletedIDs, id)
		reportMaintenance(report, "candidates", int64(i+1), int64(len(ids)))
	}
	if len(deletedIDs) == 0 {
		return MaintenanceResult{}, nil
	}
	reportMaintenance(report, "commit", 0, 0)
	for _, index := range cleanupReferenceIndexes {
		if _, err := tx.ExecContext(ctx, "DROP INDEX "+index.name); err != nil {
			return MaintenanceResult{}, fmt.Errorf("drop cleanup reference index: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return MaintenanceResult{}, fmt.Errorf("commit history cleanup: %w", err)
	}
	result.RemovedLineages = len(deletedIDs)
	result.RemovedEntries = deletedEntries
	var assetErrors error
	reportMaintenance(report, "assets", 0, 0)
	for _, id := range deletedIDs {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(assetErrors, err)
		}
		if err := removeCleanupTree(ctx, assets, id); err != nil {
			// SQL deletion has already committed for every family. Continue after
			// ordinary filesystem failures so later families do not become orphans
			// merely because an earlier directory could not be removed.
			assetErrors = errors.Join(assetErrors, fmt.Errorf("delete lineage %s assets: %w", id, err))
		}
	}
	return result, assetErrors
}

func (s *Store) refreshLoadedActivity(ctx context.Context, now time.Time, protectedSession string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET last_activity_ms=max(last_activity_ms,?) WHERE id=?", now.UnixMilli(), protectedSession)
	if err != nil {
		return fmt.Errorf("refresh loaded history activity: %w", err)
	}
	return nil
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

func deleteLineage(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	const sessions = "SELECT id FROM sessions WHERE lineage_id=?"
	queries := []string{
		"DELETE FROM compactions WHERE continuation_id IN (" + sessions + ")",
		"DELETE FROM request_observations WHERE request_id IN (SELECT id FROM model_requests WHERE session_id IN (" + sessions + "))",
		"DELETE FROM tool_records WHERE entry_id IN (SELECT id FROM entries WHERE session_id IN (" + sessions + "))",
		"DELETE FROM file_changes WHERE session_id IN (" + sessions + ")",
		"DELETE FROM tool_calls WHERE session_id IN (" + sessions + ")",
		"DELETE FROM model_requests WHERE session_id IN (" + sessions + ")",
		"DELETE FROM entries WHERE session_id IN (" + sessions + ")",
		"DELETE FROM turns WHERE session_id IN (" + sessions + ")",
		"DELETE FROM sessions WHERE lineage_id=?",
	}
	var entries int64
	for _, query := range queries {
		result, err := tx.ExecContext(ctx, query, id)
		if err != nil {
			return 0, err
		}
		if strings.HasPrefix(query, "DELETE FROM entries ") {
			entries, err = result.RowsAffected()
			if err != nil {
				return 0, fmt.Errorf("count deleted entries: %w", err)
			}
		}
	}
	return entries, nil
}

// SQLite checks child keys for each deleted parent even with deferred checks.
// Existing compound indexes do not cover a lookup on their non-leading column.
var cleanupReferenceIndexes = [...]struct{ name, table, column string }{
	{"cleanup_session_active", "sessions", "active_entry_id"},
	{"cleanup_session_redo", "sessions", "redo_entry_id"},
	{"cleanup_session_floor", "sessions", "undo_floor_id"},
	{"cleanup_session_file", "sessions", "file_tip_id"},
	{"cleanup_turn_entry", "turns", "start_entry_id"},
	{"cleanup_turn_file", "turns", "start_file_tip_id"},
	{"cleanup_entry_source", "entries", "source_id"},
	{"cleanup_entry_turn", "entries", "turn_id"},
	{"cleanup_entry_request", "entries", "delivered_request_id"},
	{"cleanup_entry_main_turn", "entries", "main_turn_id"},
	{"cleanup_entry_undo_turn", "entries", "undo_owner_turn_id"},
	{"cleanup_entry_file", "entries", "file_tip_id"},
	{"cleanup_file_previous", "file_changes", "previous_id"},
	{"cleanup_compaction_source", "compactions", "source_tip_id"},
	{"cleanup_compaction_summary", "compactions", "summary_entry_id"},
	{"cleanup_request_session", "model_requests", "session_id"},
	{"cleanup_request_admission", "model_requests", "admission_entry_id"},
	{"cleanup_request_prompt", "model_requests", "system_prompt_entry_id"},
	{"cleanup_observation_admission", "request_observations", "admission_entry_id"},
	{"cleanup_observation_prompt", "request_observations", "system_prompt_entry_id"},
}

func indexCleanupReferences(ctx context.Context, tx *sql.Tx) error {
	for _, index := range cleanupReferenceIndexes {
		if _, err := tx.ExecContext(ctx, "CREATE INDEX "+index.name+" ON "+index.table+"("+index.column+") WHERE "+index.column+" IS NOT NULL"); err != nil {
			return fmt.Errorf("index history references for cleanup: %w", err)
		}
	}
	return nil
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
