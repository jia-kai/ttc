package migrate

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// databaseEvidence includes every stored value, schema definition and root page,
// along with layout counters. JSON strings and blobs retain their exact bytes.
func databaseEvidence(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	queries := map[string]string{
		"schema":        "SELECT type,name,tbl_name,rootpage,sql FROM sqlite_schema ORDER BY name",
		"schemaVersion": "PRAGMA schema_version",
		"pageCount":     "PRAGMA page_count",
		"freePages":     "PRAGMA freelist_count",
		"changes":       "SELECT total_changes()",
	}
	rows, err := db.Query("SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		// The fixture uses ordinary SQLite rowid tables and fixed table names.
		queries["table:"+name] = fmt.Sprintf("SELECT * FROM %q ORDER BY rowid", name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	evidence := make(map[string]string, len(queries))
	for name, query := range queries {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var values [][]any
		for rows.Next() {
			row := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range row {
				pointers[i] = &row[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for i, value := range row {
				if raw, ok := value.([]byte); ok {
					row[i] = append([]byte(nil), raw...)
				}
			}
			values = append(values, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		evidence[name] = string(encoded)
	}
	return evidence
}

func TestSchema10FencePreservesAllEvidenceAndLayout(t *testing.T) {
	for _, report := range []bool{false, true} {
		for _, commit := range []bool{false, true} {
			t.Run(fmt.Sprintf("progress=%t/commit=%t", report, commit), func(t *testing.T) {
				db := fixture(t)
				applyCurrent(t, db)
				exec(t, db, "PRAGMA user_version=10")
				// Deliberate whitespace, unknown fields and non-ASCII content must
				// survive without decoding, reclassification or re-encoding.
				exec(t, db, `UPDATE entries SET content_json=? WHERE id=10`, " { \"type\":\"assistant\", \"content\":\"original summary λ\\n\", \"future\":9007199254740993 } ")
				archive := filepath.Join(t.TempDir(), "original.jsonl")
				archiveBytes := []byte(scalar(t, db, "SELECT input_json FROM model_requests WHERE id=1") + "\n")
				if err := os.WriteFile(archive, archiveBytes, 0600); err != nil {
					t.Fatal(err)
				}
				before := databaseEvidence(t, db)
				tx := transaction(t, db)
				var events []Progress
				var callback func(Progress)
				if report {
					callback = func(event Progress) { events = append(events, event) }
				}
				if err := Apply(tx, callback); err != nil {
					t.Fatal(err)
				}
				var version int
				if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 11 {
					t.Fatalf("uncommitted fence version=%d err=%v", version, err)
				}
				if report {
					want := []Progress{
						{FromVersion: 10, ToVersion: 11, Total: 1, Phase: "update_schema_version"},
						{FromVersion: 10, ToVersion: 11, Completed: 1, Total: 1, Phase: "complete"},
					}
					if !reflect.DeepEqual(events, want) {
						t.Fatalf("progress=%+v, want %+v", events, want)
					}
				}
				wantVersion := "10"
				if commit {
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
					wantVersion = "11"
				} else if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				if got := scalar(t, db, "PRAGMA user_version"); got != wantVersion {
					t.Fatalf("version=%s, want %s", got, wantVersion)
				}
				if after := databaseEvidence(t, db); !reflect.DeepEqual(before, after) {
					t.Fatalf("version-only fence changed evidence or layout:\nbefore=%v\nafter=%v", before, after)
				}
				if got, err := os.ReadFile(archive); err != nil || string(got) != string(archiveBytes) {
					t.Fatalf("archive changed: %v", err)
				}
			})
		}
	}
}

func TestSchema10FenceDoesNotRequireHistoryTables(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, "PRAGMA user_version=10")
	applyCurrent(t, db)
	if got := scalar(t, db, "PRAGMA user_version"); got != "11" {
		t.Fatal(got)
	}
	if got := scalar(t, db, "SELECT count(*) FROM sqlite_schema"); got != "0" {
		t.Fatal("version fence created schema objects", got)
	}
}
