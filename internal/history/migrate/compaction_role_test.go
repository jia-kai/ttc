package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSchema12CompactionRoleMigrationPreservesCanonicalAndAuditEvidence(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%t", commit), func(t *testing.T) {
			db := fixture(t)
			applyCurrent(t, db)
			exec(t, db, "PRAGMA user_version=12")
			const raw = ` { "type":"compaction_context", "content":"Continue using the summary below.\n\nSummary λ", "future":9007199254740993 } `
			cases := []struct {
				kind, role, body, want string
				source                 int
				visible                bool
			}{
				{"summary", "developer", raw, "user", 0, true},
				{"message", "developer", raw, "user", 0, true},
				{"summary", "developer", raw, "user", 100, false},
				{"summary", "assistant", `{"type":"assistant","content":"historical summary"}`, "assistant", 0, true},
				{"summary", "developer", `{"type":"developer_instruction","content":"compaction_context"}`, "developer", 0, true},
				{"message", "developer", `{"type":"runtime_context","content":"summary"}`, "developer", 0, true},
				{"status", "developer", `{"type":"request_message","purpose":"compaction","role":"developer","message":{"type":"compaction_context","content":"immutable request"}}`, "developer", 0, false},
				{"summary", "user", raw, "user", 0, true},
				{"summary", "developer", `{"Type":"compaction_context","content":"case-folded type"}`, "user", 0, true},
				{"message", "developer", `{"type":"developer_instruction","type":"compaction_context","content":"last type wins"}`, "user", 0, true},
				{"message", "developer", `{"type":"compaction_context","type":"developer_instruction","content":"unrelated developer policy"}`, "developer", 0, true},
				{"message", "developer", `{"type":"developer_instruction","TYPE":"compaction_context","content":"case-folded duplicate"}`, "user", 0, true},
				{"summary", "developer", `{"type":"compaction_context","TYPE":null,"content":"null does not replace type"}`, "user", 0, true},
				{"summary", "developer", `{"type":null,"Type":"compaction_context","content":"type after null"}`, "user", 0, true},
				{"message", "developer", `{"type":"developer_instruction","TYPE":null,"content":"keep developer policy"}`, "developer", 0, true},
				{"status", "developer", raw, "developer", 0, false},
			}
			for i, tc := range cases {
				exec(t, db, `INSERT INTO entries(id,session_id,source_id,actor_id,kind,role,model_visible,content_json,created_ms)
					VALUES(?,'continued',nullif(?,0),'main/child_example',?,?,?,?,1)`, 100+i, tc.source, tc.kind, tc.role, tc.visible, tc.body)
			}
			// Provider-native requests and archive files remain immutable evidence.
			exec(t, db, "UPDATE model_requests SET input_json=? WHERE id=1", raw)
			archive := filepath.Join(t.TempDir(), "saved-request.jsonl")
			if err := os.WriteFile(archive, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			before := databaseEvidence(t, db)
			tx := transaction(t, db)
			var events []Progress
			progress := &progressReporter{report: func(event Progress) { events = append(events, event) }, value: Progress{FromVersion: 12, ToVersion: 13}}
			if err := upgrade12(tx, progress); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec("PRAGMA user_version=13"); err != nil {
				t.Fatal(err)
			}
			progress.finish()
			if len(events) < 2 || events[len(events)-1].Phase != "complete" || events[len(events)-1].ToVersion != 13 {
				t.Fatal("incomplete role migration progress", events)
			}
			if commit {
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			} else if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			wantVersion := "12"
			if commit {
				wantVersion = "13"
			}
			if got := scalar(t, db, "PRAGMA user_version"); got != wantVersion {
				t.Fatalf("version=%s, want %s", got, wantVersion)
			}
			for i, tc := range cases {
				var role, body string
				var source int
				if err := db.QueryRow("SELECT role,content_json,coalesce(source_id,0) FROM entries WHERE id=?", 100+i).Scan(&role, &body, &source); err != nil {
					t.Fatal(err)
				}
				want := tc.role
				if commit {
					want = tc.want
				}
				if role != want || body != tc.body || source != tc.source {
					t.Fatalf("entry %d: role=%q want=%q content=%q source=%d", 100+i, role, want, body, source)
				}
			}
			after := databaseEvidence(t, db)
			// SQLite total_changes counts rolled-back writes too. A committed
			// migration changes only the tested entries.role index values.
			delete(before, "changes")
			delete(after, "changes")
			if commit {
				delete(before, "table:entries")
				delete(after, "table:entries")
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("migration changed unrelated evidence/layout:\nbefore=%v\nafter=%v", before, after)
			}
			if got, err := os.ReadFile(archive); err != nil || string(got) != raw {
				t.Fatal("migration changed archived request bytes", string(got), err)
			}
		})
	}
}
