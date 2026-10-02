package history

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scicode/internal/provider"
	"scicode/internal/render"
)

func ageLineage(t *testing.T, s *Store, lineage string, at time.Time) {
	t.Helper()
	if _, err := s.DB.Exec("UPDATE sessions SET last_activity_ms=? WHERE lineage_id=?", at.UnixMilli(), lineage); err != nil {
		t.Fatal(err)
	}
}

func retainedContinuation(t *testing.T, s *Store, v Session) Session {
	t.Helper()
	saved, err := s.Session(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := s.ArchiveTranscript(v.ID, saved.EntryTip)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Continue(v.ID, "Saved research handoff.", archive, saved.EntryTip, 0)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestCleanupDeletesWholeLineageAndPreservesUserState(t *testing.T) {
	s, v, turn, req := historyFixture(t)
	call := provider.ToolCall{ID: "write", Name: "write", Arguments: []byte(`{"path":"result.txt","content":"result"}`)}
	_, calls, err := s.Assistant(v.ID, turn, "main", req, provider.Message{Role: "assistant", Calls: []provider.ToolCall{call}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Prepare(v.ID, "apply", map[string]string{"path": "result.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitChange(v.ID, calls[0], []string{"result.txt"}, true); err != nil {
		t.Fatal(err)
	}
	result := []byte(`{"path":"result.txt","ok":true}`)
	if _, err := s.CallResult(v.ID, turn, "main", calls[0], result, map[string]string{"name": "write"}, render.Tool("write", call.Arguments, result), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSystemPrompt(v.ID, turn, "main", req, "Exact instructions."); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestMessage(v.ID, turn, "main/child", "assistant", req, provider.Message{Role: "assistant", Content: "Child output"}); err != nil {
		t.Fatal(err)
	}
	for _, category := range []string{"snapshots", "images", "attachments", "details"} {
		if _, err := s.Artifact(v.ID, category, []byte("retained bytes")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	next := retainedContinuation(t, s, v)
	other, _ := startHistorySession(t, s, t.TempDir(), v.Model, "Keep recent work")
	now := time.Now()
	ageLineage(t, s, v.LineageID, now.Add(-lineageRetention))
	credential := filepath.Join(s.Root, "openai-auth.json")
	if err := os.WriteFile(credential, []byte("credentials stay"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSelection(v.Model); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Cleanup(context.Background(), now, other.ID)
	if err != nil || removed != 1 {
		t.Fatal(removed, err)
	}
	for _, id := range []string{v.ID, next.ID} {
		if _, err := s.Session(id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("expired session survived", id, err)
		}
	}
	for _, table := range []string{"tool_records", "file_changes", "tool_calls", "compactions"} {
		var count int
		if err := s.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
	for _, table := range []string{"entries", "turns", "model_requests"} {
		var count int
		if err := s.DB.QueryRow("SELECT count(*) FROM "+table+" WHERE session_id IN (?,?)", v.ID, next.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(s.Root, "lineages", v.LineageID)); !os.IsNotExist(err) {
		t.Fatal("expired assets survived", err)
	}
	if b, err := os.ReadFile(credential); err != nil || string(b) != "credentials stay" {
		t.Fatal("credentials removed", string(b), err)
	}
	if choice, err := s.LastSelection("script"); err != nil || choice == nil {
		t.Fatal("model preferences removed", choice, err)
	}
	var generation int64
	if err := s.DB.QueryRow("SELECT generation FROM workspaces WHERE id=?", v.WorkspaceID).Scan(&generation); err != nil || generation != 1 {
		t.Fatal("workspace generation removed", generation, err)
	}
	rows, err := s.DB.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("dangling lineage references")
	}
}

func TestCleanupProtectsLoadedAndPendingLineages(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	next := retainedContinuation(t, s, v)
	now := time.Now()
	ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
	if removed, err := s.Cleanup(context.Background(), now, v.ID); err != nil || removed != 0 {
		t.Fatal("loaded predecessor did not protect descendants", removed, err)
	}
	if err := s.Prepare(next.ID, "apply", map[string]string{"pending": "operation"}); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.Cleanup(context.Background(), now, ""); err != nil || removed != 0 {
		t.Fatal("pending journal did not protect lineage", removed, err)
	}
	if err := s.ClearOperation(next.ID); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.Cleanup(context.Background(), now, "unsaved-session"); err != nil || removed != 1 {
		t.Fatal(removed, err)
	}
}

func TestCleanupUsesLatestActivityAcrossContinuations(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	next := retainedContinuation(t, s, v)
	now := time.Now()
	ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
	if _, err := s.DB.Exec("UPDATE sessions SET last_activity_ms=? WHERE id=?", now.Add(-29*24*time.Hour).UnixMilli(), next.ID); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.Cleanup(context.Background(), now, ""); err != nil || removed != 0 {
		t.Fatal("recent continuation lost its predecessor", removed, err)
	}
}

func TestCleanupRetriesInterruptedAssetDeletion(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	path, err := s.Artifact(v.ID, "snapshots", []byte("snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
	if err := os.Chmod(filepath.Dir(path), 0500); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.Cleanup(context.Background(), now, ""); err == nil || removed != 0 || !strings.Contains(err.Error(), "retry marker retained") {
		t.Fatal("unsafe asset directory error hidden", removed, err)
	}
	marker := filepath.Join(s.Root, ".retention", v.LineageID)
	if st, err := os.Stat(marker); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("deletion marker missing", st, err)
	}
	if _, err := s.Session(v.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("SQL deletion was not committed", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.Cleanup(context.Background(), now, ""); err != nil || removed != 1 {
		t.Fatal("asset retry failed", removed, err)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("completed marker survived", err)
	}
}

func TestCleanupSQLRollbackAndRecentMarkerProtection(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	now := time.Now()
	ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
	if _, err := s.DB.Exec("CREATE TRIGGER reject_cleanup BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT,'controlled cleanup failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cleanup(context.Background(), now, ""); err == nil || !strings.Contains(err.Error(), "controlled cleanup failure") {
		t.Fatal("SQL deletion failure hidden", err)
	}
	if _, err := s.Session(v.ID); err != nil {
		t.Fatal("failed transaction lost history", err)
	}
	if _, err := s.DB.Exec("DROP TRIGGER reject_cleanup"); err != nil {
		t.Fatal(err)
	}
	ageLineage(t, s, v.LineageID, now)
	if removed, err := s.Cleanup(context.Background(), now, ""); err != nil || removed != 0 {
		t.Fatal("retry marker deleted recently used history", removed, err)
	}
	ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
	if removed, err := s.Cleanup(context.Background(), now, ""); err != nil || removed != 1 {
		t.Fatal(removed, err)
	}
}

func TestCleanupNeverFollowsManagedSymlinks(t *testing.T) {
	for _, location := range []string{"lineages", ".retention", "lineage", "nested"} {
		t.Run(location, func(t *testing.T) {
			s, v, _, _ := historyFixture(t)
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(sentinel, []byte("keep outside bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(s.Root, location)
			if location == "lineage" || location == "nested" {
				if err := PrivateDir(filepath.Join(s.Root, "lineages")); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(s.Root, "lineages", v.LineageID)
				if location == "nested" {
					if err := PrivateDir(link); err != nil {
						t.Fatal(err)
					}
					link = filepath.Join(link, "snapshot-link")
				}
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
			removed, err := s.Cleanup(context.Background(), now, "")
			if location == "lineages" || location == ".retention" {
				if err == nil || removed != 0 {
					t.Fatal("followed managed parent symlink", removed, err)
				}
			} else if err != nil || removed != 1 {
				t.Fatal("failed to unlink symlink itself", removed, err)
			}
			if b, err := os.ReadFile(sentinel); err != nil || string(b) != "keep outside bytes" {
				t.Fatal("outside tree changed", string(b), err)
			}
		})
	}
}

func TestCleanupRejectsInvalidMarkersAndHonorsCancellation(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	if err := PrivateDir(filepath.Join(s.Root, ".retention")); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(s.Root, ".retention", v.LineageID)
	if err := os.Symlink(filepath.Join(s.Root, "history.sqlite"), marker); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
	if _, err := s.Cleanup(context.Background(), now, ""); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatal("invalid deletion marker accepted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Cleanup(ctx, now, ""); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	if _, err := s.Session(v.ID); err != nil {
		t.Fatal("invalid marker damaged history", err)
	}
}

func TestCleanupRejectsLineagePathTraversal(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	now := time.Now()
	if _, err := s.DB.Exec("UPDATE sessions SET lineage_id='../outside',last_activity_ms=? WHERE id=?", now.Add(-31*24*time.Hour).UnixMilli(), v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cleanup(context.Background(), now, ""); err == nil || !strings.Contains(err.Error(), "invalid retention lineage") {
		t.Fatal("unsafe lineage path accepted", err)
	}
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM sessions WHERE id=?", v.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("unsafe lineage altered SQL history", count, err)
	}
}
