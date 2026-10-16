package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
)

func testSelection() llm.Selection {
	return llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
}

func assertNoCleanupIndexes(t *testing.T, s *Store) {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='index' AND name GLOB 'cleanup_*'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("transient cleanup indexes survived", count, err)
	}
}

func TestMaintainThresholdBoundaries(t *testing.T) {
	for _, delta := range []int64{1, 0, -1} {
		t.Run(fmt.Sprintf("threshold-minus-size=%d", delta), func(t *testing.T) {
			s, v, _, _ := historyFixture(t)
			now := time.Now()
			ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
			size := physicalHistoryBytes(t, s)
			result, err := s.maintain(context.Background(), now, "", size+delta)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if delta > 0 {
				want = 0
			}
			if result.RemovedLineages != want || result.Stats.Bytes != physicalHistoryBytes(t, s) {
				t.Fatal(result, "want removed", want)
			}
			_, err = s.Session(v.ID)
			if want == 0 && err != nil || want == 1 && !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("wrong threshold decision", err)
			}
			assertNoCleanupIndexes(t, s)
		})
	}
}

func TestMaintainBelowProductionThresholdRefreshesLoadedFamily(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	now := time.Now()
	ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
	result, err := s.Maintain(context.Background(), now, v.ID)
	if err != nil || result.RemovedLineages != 0 || result.Stats.Bytes >= historyMaintenanceThreshold {
		t.Fatal(result, err)
	}
	saved, err := s.Session(v.ID)
	if err != nil || saved.LastActivityMS != now.UnixMilli() {
		t.Fatal("loaded session activity not refreshed", saved.LastActivityMS, err)
	}
	other, err := Open(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if removed, err := other.maintain(context.Background(), now, "", 1); err != nil || removed.RemovedLineages != 0 {
		t.Fatal("another instance deleted the loaded family", removed, err)
	}
}

func TestRefreshLoadedActivityIsCheapMonotonicAndCancelable(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	now := time.Now()
	ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
	if err := s.RefreshLoadedActivity(context.Background(), now, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshLoadedActivity(context.Background(), now.Add(-time.Hour), v.ID); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Session(v.ID)
	if err != nil || saved.LastActivityMS != now.UnixMilli() {
		t.Fatal("activity moved backward", saved.LastActivityMS, err)
	}
	if err := s.RefreshLoadedActivity(context.Background(), now, "unsaved-session"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.RefreshLoadedActivity(ctx, now.Add(time.Hour), v.ID); !errors.Is(err, context.Canceled) {
		t.Fatal("refresh ignored cancellation", err)
	}
}

func TestMaintainPurgesOnlyOldUnprotectedWholeFamilies(t *testing.T) {
	s, old, oldTurn, _ := historyFixture(t)
	if err := s.FinishTurn(oldTurn, "completed"); err != nil {
		t.Fatal(err)
	}
	oldNext := retainedContinuation(t, s, old)
	protected, protectedTurn := startHistorySession(t, s, t.TempDir(), old.Model, "protected")
	if err := s.FinishTurn(protectedTurn, "completed"); err != nil {
		t.Fatal(err)
	}
	protectedNext := retainedContinuation(t, s, protected)
	recent, recentTurn := startHistorySession(t, s, t.TempDir(), old.Model, strings.Repeat("recent payload", 10000))
	if err := s.FinishTurn(recentTurn, "completed"); err != nil {
		t.Fatal(err)
	}
	recentNext := retainedContinuation(t, s, recent)
	now := time.Now()
	for _, v := range []Session{old, protected, recent} {
		ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
	}
	// One recent descendant protects the whole recent family.
	if _, err := s.db.Exec("UPDATE sessions SET last_activity_ms=? WHERE id=?", now.Add(-29*24*time.Hour).UnixMilli(), recentNext.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"user-export.md", "openai-auth.json"} {
		if err := os.WriteFile(filepath.Join(s.Root, name), []byte("keep bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const survivingEntries = `SELECT json_group_array(json_array(id,session_id,parent_id,source_id,content_json,created_ms)) FROM (SELECT * FROM entries WHERE session_id IN (?,?,?,?) ORDER BY id)`
	var beforeEntries string
	if err := s.db.QueryRow(survivingEntries, protected.ID, protectedNext.ID, recent.ID, recentNext.ID).Scan(&beforeEntries); err != nil {
		t.Fatal(err)
	}
	result, err := s.maintain(context.Background(), now, protected.ID, 1)
	if err != nil || result.RemovedLineages != 1 || result.Stats.Bytes <= 1 {
		t.Fatal("purge must stop even while still over threshold", result, err)
	}
	for _, id := range []string{old.ID, oldNext.ID} {
		if _, err := s.Session(id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("old family survived", id, err)
		}
	}
	for _, id := range []string{protected.ID, protectedNext.ID, recent.ID, recentNext.ID} {
		if _, err := s.Session(id); err != nil {
			t.Fatal("protected/recent family lost", id, err)
		}
	}
	var afterEntries string
	if err := s.db.QueryRow(survivingEntries, protected.ID, protectedNext.ID, recent.ID, recentNext.ID).Scan(&afterEntries); err != nil || afterEntries != beforeEntries {
		t.Fatal("maintenance changed retained entries or their identities", err)
	}
	for _, name := range []string{"user-export.md", "openai-auth.json"} {
		if data, err := os.ReadFile(filepath.Join(s.Root, name)); err != nil || string(data) != "keep bytes" {
			t.Fatal("unmanaged user state altered", name, err)
		}
	}
	assertNoCleanupIndexes(t, s)
}

func TestMaintainNoEligibleOrFreePagesDoesNotVacuum(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	if _, err := s.db.Exec("UPDATE entries SET content_json=json_set(content_json,'$.content',?) WHERE session_id=?", strings.Repeat("active payload", 100000), v.ID); err != nil {
		t.Fatal(err)
	}
	var beforeVersion, freePages int64
	if err := s.db.QueryRow("PRAGMA schema_version").Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA freelist_count").Scan(&freePages); err != nil || freePages != 0 {
		t.Fatal("active fixture unexpectedly has free pages", freePages, err)
	}
	result, err := s.maintain(context.Background(), time.Now(), "", 1)
	if err != nil || result.RemovedLineages != 0 || result.Stats.Bytes <= 1 {
		t.Fatal(result, err)
	}
	// VACUUM increments schema_version, unlike a checkpoint. This also catches
	// needless full compaction when a large active database stays over threshold.
	var afterVersion int64
	if err := s.db.QueryRow("PRAGMA schema_version").Scan(&afterVersion); err != nil || afterVersion != beforeVersion {
		t.Fatal("active database was needlessly vacuumed", beforeVersion, afterVersion, err)
	}
	if _, err := s.Session(v.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMaintainReclaimsDiskAndPreservesIdentityHighWater(t *testing.T) {
	s, old, _, _ := historyFixture(t)
	keep, _ := startHistorySession(t, s, t.TempDir(), old.Model, "keep")
	if _, err := s.db.Exec("UPDATE entries SET content_json=json_set(content_json,'$.content',?) WHERE session_id=?", strings.Repeat("large old payload", 100000), old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE sqlite_sequence SET seq=1000000 WHERE name='entries'"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ageLineage(t, s, old.LineageID, now.Add(-31*24*time.Hour))
	before := physicalHistoryBytes(t, s)
	result, err := s.maintain(context.Background(), now, keep.ID, 1)
	if err != nil || result.RemovedLineages != 1 || result.Stats.Bytes >= before/2 {
		t.Fatal("disk was not reclaimed", before, result, err)
	}
	var highWater int64
	if err := s.db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='entries'").Scan(&highWater); err != nil || highWater != 1000000 {
		t.Fatal("VACUUM lost entry identity high-water mark", highWater, err)
	}
}

func TestMaintainSQLFailureRollsBackAllFamiliesAndIndexes(t *testing.T) {
	s, first, _, _ := historyFixture(t)
	second, _ := startHistorySession(t, s, t.TempDir(), first.Model, "second old family")
	now := time.Now()
	for _, v := range []Session{first, second} {
		ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
	}
	later := first.ID
	if second.ID > later {
		later = second.ID
	}
	if _, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER reject_maintenance BEFORE DELETE ON sessions WHEN OLD.id='%s' BEGIN SELECT RAISE(ABORT,'maintenance failure'); END", later)); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := s.db.QueryRow("SELECT count(*) FROM entries").Scan(&before); err != nil {
		t.Fatal(err)
	}
	result, err := s.maintain(context.Background(), now, "", 1)
	if err == nil || !strings.Contains(err.Error(), "maintenance failure") || result.RemovedLineages != 0 {
		t.Fatal(result, err)
	}
	for _, v := range []Session{first, second} {
		if _, err := s.Session(v.ID); err != nil {
			t.Fatal("failed sweep partially committed", err)
		}
	}
	var after int64
	if err := s.db.QueryRow("SELECT count(*) FROM entries").Scan(&after); err != nil || after != before {
		t.Fatal("failed sweep lost entries", after, before, err)
	}
	assertNoCleanupIndexes(t, s)
}

func TestMaintainCancellation(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	now := time.Now()
	old := now.Add(-31 * 24 * time.Hour)
	ageLineage(t, s, v.LineageID, old)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.maintain(ctx, now, v.ID, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	saved, err := s.Session(v.ID)
	if err != nil || saved.LastActivityMS != old.UnixMilli() {
		t.Fatal("cancelled maintenance changed history", saved.LastActivityMS, err)
	}
	if err := s.reclaimHistory(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatal("reclamation ignored cancellation", err)
	}
}

func TestMaintainRetriesReclamationAfterBusyCheckpoint(t *testing.T) {
	s, old, _, _ := historyFixture(t)
	if _, err := s.db.Exec("UPDATE entries SET content_json=json_set(content_json,'$.content',?)", strings.Repeat("old payload", 100000)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ageLineage(t, s, old.LineageID, now.Add(-31*24*time.Hour))
	// A raw SQLite reader models clients outside Store's lifetime-lock protocol;
	// SQLite contention must still fail safely and remain retryable.
	reader, err := openDatabase(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := s.db.Exec("PRAGMA busy_timeout=1"); err != nil {
		t.Fatal(err)
	}
	rows, err := reader.Query("SELECT id FROM entries")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("fixture has no read snapshot")
	}
	result, err := s.maintain(context.Background(), now, "", 1)
	if err == nil || !strings.Contains(err.Error(), "database busy") || result.RemovedLineages != 1 {
		t.Fatal("expected committed purge followed by busy checkpoint", result, err)
	}
	afterFailure := physicalHistoryBytes(t, s)
	rows.Close()
	// No eligible SQL rows remain, but free pages still identify the unfinished
	// reclamation. The next invocation must retry rather than leave a bloated DB.
	result, err = s.maintain(context.Background(), now, "", 1)
	if err != nil || result.RemovedLineages != 0 || result.Stats.Bytes >= afterFailure/2 || result.Stats.Entries != 0 {
		t.Fatal("reclamation was not retried after reader release", afterFailure, result, err)
	}
}

func TestMaintainRetriesWALOnlyCheckpointWithoutVacuum(t *testing.T) {
	s, active, _, _ := historyFixture(t)
	reader, err := Open(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rows, err := reader.db.Query("SELECT id FROM entries")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("fixture has no read snapshot")
	}
	if _, err := s.db.Exec("PRAGMA busy_timeout=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE entries SET content_json=json_set(content_json,'$.content',?) WHERE session_id=?", strings.Repeat("active payload", 100000), active.ID); err != nil {
		t.Fatal(err)
	}
	var beforeVersion, freePages int64
	if err := s.db.QueryRow("PRAGMA schema_version").Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA freelist_count").Scan(&freePages); err != nil || freePages != 0 {
		t.Fatal("fixture must have WAL residue but no free pages", freePages, err)
	}
	result, err := s.maintain(context.Background(), time.Now(), "", 1)
	if err == nil || !strings.Contains(err.Error(), "database busy") || result.RemovedLineages != 0 {
		t.Fatal("expected busy checkpoint without deletion", result, err)
	}
	rows.Close()
	result, err = s.maintain(context.Background(), time.Now(), "", 1)
	if err != nil || result.RemovedLineages != 0 || result.Stats.Entries == 0 {
		t.Fatal("WAL-only retry failed", result, err)
	}
	if wal, err := os.Stat(filepath.Join(s.Root, "history.sqlite-wal")); err != nil || wal.Size() != 0 {
		t.Fatal("WAL was not truncated after reader release", wal, err)
	}
	var afterVersion int64
	if err := s.db.QueryRow("PRAGMA schema_version").Scan(&afterVersion); err != nil || afterVersion != beforeVersion {
		t.Fatal("WAL-only retry needlessly vacuumed active history", beforeVersion, afterVersion, err)
	}
}

func TestCleanupIndexesCoverEveryDeletedParentLookup(t *testing.T) {
	s, _, _, _ := historyFixture(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := indexCleanupReferences(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sessions", "turns", "entries", "model_requests", "tool_calls", "tool_records", "file_changes", "compactions"} {
		rows, err := tx.Query(`SELECT "from" FROM pragma_foreign_key_list(?) WHERE "table"!='workspaces'`, table)
		if err != nil {
			t.Fatal(err)
		}
		var columns []string
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			columns = append(columns, column)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, column := range columns {
			var covered bool
			// An INTEGER PRIMARY KEY is a direct rowid lookup, without a
			// separate index; every other child key needs a leading index.
			if err := tx.QueryRow(`SELECT
				EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name=? AND type='INTEGER' AND pk=1)
				OR EXISTS(SELECT 1 FROM pragma_index_list(?) il JOIN pragma_index_info(il.name) ii ON ii.seqno=0 WHERE ii.name=?)`, table, column, table, column).Scan(&covered); err != nil {
				t.Fatal(err)
			}
			if !covered {
				t.Errorf("cleanup can repeatedly scan %s for unindexed child key %s", table, column)
			}
		}
	}
}

func TestMaintainContinuesAssetsAndReclaimsAfterAssetFailure(t *testing.T) {
	s, first, _, _ := historyFixture(t)
	second, _ := startHistorySession(t, s, t.TempDir(), first.Model, "second old family")
	bad, good := first, second
	if bad.LineageID > good.LineageID {
		bad, good = good, bad
	}
	for _, v := range []Session{bad, good} {
		dir := filepath.Join(s.Root, "lineages", v.LineageID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "asset"), []byte("managed bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The first sorted family fails the private-directory safety check. Its
	// failure must not prevent cleanup of the later family's managed assets.
	if err := os.Chmod(filepath.Join(s.Root, "lineages", bad.LineageID), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE entries SET content_json=json_set(content_json,'$.content',?) WHERE session_id=?", strings.Repeat("old payload", 100000), bad.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, v := range []Session{bad, good} {
		ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
	}
	before := physicalHistoryBytes(t, s)
	result, err := s.maintain(context.Background(), now, "", 1)
	if err == nil || !strings.Contains(err.Error(), "mode 0700") || result.RemovedLineages != 2 {
		t.Fatal("asset failure or committed deletion count lost", result, err)
	}
	for _, v := range []Session{bad, good} {
		if _, err := s.Session(v.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("asset failure did not leave committed deletion", v.ID, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Root, "lineages", good.LineageID)); !os.IsNotExist(err) {
		t.Fatal("later family assets were orphaned", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "lineages", bad.LineageID, "asset")); err != nil {
		t.Fatal("unsafe directory was traversed", err)
	}
	if after := physicalHistoryBytes(t, s); after >= before/2 {
		t.Fatal("asset error prevented database reclamation", before, after)
	}
	assertNoCleanupIndexes(t, s)
}
