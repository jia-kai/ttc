package migrate

import (
	"database/sql"
	"strings"
	"testing"
)

// resetSchema9 restores the previous index boundary without changing evidence.
func resetSchema9(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, name := range []string{"entry_created", "entry_prompt_recall", "session_list_workspace"} {
		exec(t, db, "DROP INDEX "+name)
	}
	exec(t, db, "CREATE INDEX session_list ON sessions(last_activity_ms DESC,id)")
	exec(t, db, "PRAGMA user_version=9")
}

func TestStartupIndexesMigrationProgressAndEvidence(t *testing.T) {
	db := fixture(t)
	applyCurrent(t, db)
	resetSchema9(t, db)
	before := scalar(t, db, "SELECT json_group_array(json_array(id,source_id,content_json)) FROM entries")
	tx := transaction(t, db)
	var events []Progress
	if err := Apply(tx, func(event Progress) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	checkProgress(t, events, 9)
	want := []string{"index_entry_created", "index_prompt_recall", "index_workspace_sessions", "remove_global_session_index", "update_schema_version", "complete"}
	if len(events) != len(want) {
		t.Fatalf("progress %+v, want phases %v", events, want)
	}
	for i, event := range events {
		if event.Phase != want[i] || event.Completed != int64(i) || event.Total != 5 {
			t.Fatalf("wrong phase or SQL operation count at %d: %+v", i, event)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, db, "SELECT json_group_array(json_array(id,source_id,content_json)) FROM entries"); got != before {
		t.Fatal("index migration changed saved entries")
	}
	if got := scalar(t, db, "SELECT count(*) FROM sqlite_schema WHERE name='session_list'"); got != "0" {
		t.Fatal("unused global session index remains")
	}
	if got := scalar(t, db, "SELECT sql FROM sqlite_schema WHERE name='entry_prompt_recall'"); !strings.Contains(got, "json_extract(content_json,'$.type')='user_input'") {
		t.Fatal("recall index admits nonsemantic user records", got)
	}
}

func TestStartupIndexesMigrationFailureRollsBack(t *testing.T) {
	db := fixture(t)
	applyCurrent(t, db)
	resetSchema9(t, db)
	// Force failure after the timestamp index has been built.
	exec(t, db, "CREATE INDEX entry_prompt_recall ON entries(id)")
	tx := transaction(t, db)
	var events []Progress
	err := Apply(tx, func(event Progress) { events = append(events, event) })
	if err == nil || !strings.Contains(err.Error(), "schema 9 to 10") {
		t.Fatal("expected second-index failure", err)
	}
	for _, event := range events {
		if event.Phase == "complete" {
			t.Fatal("failed migration reported completion")
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, db, "SELECT count(*) FROM sqlite_schema WHERE name='entry_created'"); got != "0" {
		t.Fatal("rollback retained partially built index")
	}
	if got := scalar(t, db, "PRAGMA user_version"); got != "9" {
		t.Fatal("failed migration changed version", got)
	}
}
