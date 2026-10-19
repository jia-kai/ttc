package history

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/history/migrate"
)

func explainStartup(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plans []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plans, "\n")
}

func TestStartupQueryIndexes(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Test the actual combined statistics query: each scalar subquery must
	// seek its endpoint rather than aggregate over the entire entries table.
	plan := explainStartup(t, s.db, storageEntryStatsQuery)
	if strings.Count(plan, "SEARCH entries") != 3 || strings.Contains(plan, "SCAN entries") ||
		!strings.Contains(plan, "COVERING INDEX entry_created") {
		t.Fatal("entry statistics must use three indexed endpoint searches", plan)
	}
	// Use PromptHistory's exact SQL, including its semantic JSON type predicate.
	// SQL LIMIT bounds returned prompts, not candidate turn lookups:
	// an arbitrarily long suffix whose turns fail the trigger filter still has
	// to be visited, but unrelated entries never enter this partial index.
	plan = explainStartup(t, s.db, promptHistoryQuery, MaxPromptHistoryBytes+1, MaxPromptHistoryEntries)
	if !strings.Contains(plan, "USING INDEX entry_prompt_recall") || strings.Contains(plan, "SCAN e\n") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatal("recall must visit only indexed authored-prompt candidates in order", plan)
	}
	// Match Sessions' projection as well as its workspace join and ordering.
	plan = explainStartup(t, s.db, sessionListQuery, "/workspace")
	if !strings.Contains(plan, "SEARCH s USING INDEX session_list_workspace (workspace_id=?)") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatal("sessions must seek directly into workspace-ordered history", plan)
	}
}

func schemaIndexDefinitions(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query("SELECT name,sql FROM sqlite_schema WHERE type='index' AND sql IS NOT NULL ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatal(err)
		}
		out[name] = strings.ToLower(strings.Join(strings.Fields(ddl), ""))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigratedSchemaIndexesMatchFreshSchema(t *testing.T) {
	fresh, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	legacy, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.Exec(schema5Fixture); err != nil {
		t.Fatal(err)
	}
	tx, err := legacy.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := migrate.Apply(tx, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	want, got := schemaIndexDefinitions(t, fresh.db), schemaIndexDefinitions(t, legacy)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated index definitions differ from fresh schema\ngot: %v\nwant: %v", got, want)
	}
}
