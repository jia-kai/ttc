package migrate

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

//go:embed testdata/schema5.sql
var schema5 string

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	exec(t, db, "PRAGMA foreign_keys=ON")
	exec(t, db, schema5)
	for _, id := range []string{"old", "continued", "copy", "cut"} {
		exec(t, db, `INSERT OR IGNORE INTO workspaces VALUES('w','/workspace')`)
		exec(t, db, `INSERT INTO sessions(id,workspace_id,lineage_id,name,name_source,model_json,last_activity_ms,metadata_json) VALUES(?,'w','old',?,'default','{}',1,'{}')`, id, id)
	}
	exec(t, db, `UPDATE sessions SET read_only=1 WHERE id='old'`)
	exec(t, db, `INSERT INTO turns(id,session_id,trigger,status,model_json,started_ms) VALUES('normal','old','user','completed','{}',100),('steer','old','steer','completed','{}',100)`)
	// Original human admissions are not retained themselves. Expanded text and
	// repeated attachments are genuine old canonical llm.Message JSON.
	human := `{"role":"user","content":"exact expanded\ntext","files":[{"path":"/a.png","sha256":"abc","bytes":8},{"path":"/a.png","sha256":"abc","bytes":8}],"input_source":"queue"}`
	insert(t, db, 1, "old", 0, 0, "normal", "message", "user", human, 100)
	insert(t, db, 2, "old", 1, 0, "steer", "message", "user", `{"role":"user","content":"interrupt"}`, 110)
	insert(t, db, 10, "continued", 0, 0, "", "summary", "assistant", `{"role":"assistant","content":"summary"}`, 200)
	insert(t, db, 11, "continued", 10, 0, "", "message", "developer", marker(t, "queue", 100, 200), 200)
	insert(t, db, 12, "continued", 11, 1, "normal", "message", "user", human, 200)
	insert(t, db, 13, "continued", 12, 0, "", "message", "developer", marker(t, "steer", 110, 200), 200)
	insert(t, db, 14, "continued", 13, 2, "steer", "message", "user", `{"role":"user","content":"interrupt","input_source":"steer","input_time_ms":110}`, 200)
	// A side branch also contains a retained pair.
	insert(t, db, 15, "continued", 10, 0, "", "message", "developer", marker(t, "queue", 100, 220), 220)
	insert(t, db, 16, "continued", 15, 1, "normal", "message", "user", human, 220)
	insert(t, db, 20, "copy", 0, 0, "", "summary", "assistant", `{"role":"assistant","content":"loaded summary"}`, 300)
	insert(t, db, 21, "copy", 20, 11, "", "message", "developer", marker(t, "queue", 100, 200), 300)
	insert(t, db, 22, "copy", 21, 1, "normal", "message", "user", human, 300)
	// A legally copied pre-input cursor has no local following human. Its
	// immutable source marker and original child supply provenance evidence.
	insert(t, db, 30, "cut", 0, 0, "", "summary", "assistant", `{"role":"assistant","content":"cut summary"}`, 300)
	insert(t, db, 31, "cut", 30, 11, "", "message", "developer", marker(t, "queue", 100, 200), 300)
	exec(t, db, `INSERT INTO model_requests(id,session_id,actor_id,input_json,purpose,model_json,status,attempts_json,created_ms) VALUES(1,'old','main',?,'coding','{}','completed','[]',100)`, "[ "+marker(t, "queue", 100, 200)+", "+human+" ]")
	exec(t, db, `INSERT INTO tool_calls VALUES('call','old',1,'provider','main','edit',1,'{}','{"ok":true}')`)
	exec(t, db, `INSERT INTO file_changes VALUES(1,'old',NULL,'call','["file"]',1,100)`)
	exec(t, db, `UPDATE entries SET file_tip_id=1`)
	exec(t, db, `UPDATE sessions SET active_entry_id=31,redo_entry_id=31,undo_floor_id=31,file_tip_id=1 WHERE id='cut'`)
	exec(t, db, `UPDATE sessions SET active_entry_id=14,file_tip_id=1 WHERE id='continued'`)
	exec(t, db, `UPDATE turns SET start_entry_id=31,start_file_tip_id=1 WHERE id='normal'`)
	exec(t, db, `INSERT INTO compactions VALUES('continued','old',2,0,'archive.md','markdown-hash','exact-hash',10)`)
	return db
}
func insert(t *testing.T, db *sql.DB, id int64, session string, parent, source int64, turn, kind, role, raw string, created int64) {
	t.Helper()
	exec(t, db, `INSERT INTO entries(id,session_id,parent_id,source_id,turn_id,actor_id,kind,role,model_visible,content_json,created_ms) VALUES(?,?,NULLIF(?,0),NULLIF(?,0),NULLIF(?,''),'main',?,?,1,?,?)`, id, session, parent, source, turn, kind, role, raw, created)
}
func marker(t *testing.T, source string, original, compacted int64) string {
	t.Helper()
	age := compacted - original
	m, _ := json.Marshal(metadata{"retained_input", source, original, compacted, &age})
	raw, err := json.Marshal(map[string]string{"role": "developer", "content": legacyMarker + "\n" + string(m)})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
func scalar(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var value string
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

// applySchema5 isolates retention migration assertions from later payload codecs.
func applySchema5(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version == 5 {
		if err = upgrade5(tx, nil); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec("PRAGMA user_version=6"); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSchema5MigrationPreservesEvidenceAndCursors(t *testing.T) {
	db := fixture(t)
	request := scalar(t, db, "SELECT input_json FROM model_requests WHERE id=1")
	originals := scalar(t, db, "SELECT json_group_array(content_json) FROM entries WHERE id IN (1,2)")
	relations := scalar(t, db, "SELECT json_group_array(json_array(id,parent_id,source_id,session_id,turn_id,file_tip_id)) FROM entries")
	archive := filepath.Join(t.TempDir(), "archive.jsonl")
	archiveBytes := []byte(request + "\n")
	if err := os.WriteFile(archive, archiveBytes, 0600); err != nil {
		t.Fatal(err)
	}
	applySchema5(t, db)
	if got := scalar(t, db, "PRAGMA user_version"); got != "6" {
		t.Fatal(got)
	}
	for _, id := range []int{12, 14, 16, 22} {
		raw := scalar(t, db, "SELECT content_json FROM entries WHERE id=?", id)
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if text(m, "role") != "user" || len(m["retained_at_ms"]) == 0 || len(m["input_time_ms"]) == 0 {
			t.Fatal(id, raw)
		}
	}
	if got := scalar(t, db, "SELECT json_extract(content_json,'$.input_source') FROM entries WHERE id=14"); got != "steer" {
		t.Fatal(got)
	}
	if got := scalar(t, db, "SELECT json_extract(content_json,'$.retained_at_ms') FROM entries WHERE id=16"); got != "220" {
		t.Fatal(got)
	}
	if got := scalar(t, db, "SELECT json_extract(content_json,'$.files') FROM entries WHERE id=12"); got != scalar(t, db, "SELECT json_extract(content_json,'$.files') FROM entries WHERE id=1") {
		t.Fatal("attachments changed", got)
	}
	if got := scalar(t, db, "SELECT count(*) FROM entries WHERE id IN (11,13,15,21,31) AND kind='status' AND role IS NULL AND model_visible=0 AND json_extract(content_json,'$.type')='retention_boundary'"); got != "5" {
		t.Fatal(got)
	}
	if got := scalar(t, db, "SELECT json_array(active_entry_id,redo_entry_id,undo_floor_id,file_tip_id) FROM sessions WHERE id='cut'"); got != "[30,30,30,1]" {
		t.Fatal(got)
	}
	if got := scalar(t, db, "SELECT json_array(start_entry_id,start_file_tip_id) FROM turns WHERE id='normal'"); got != "[30,1]" {
		t.Fatal(got)
	}
	if got := scalar(t, db, "SELECT read_only FROM sessions WHERE id='old'"); got != "1" {
		t.Fatal(got)
	}
	if got := scalar(t, db, "SELECT input_json FROM model_requests WHERE id=1"); got != request {
		t.Fatal("request evidence changed")
	}
	if got := scalar(t, db, "SELECT json_group_array(content_json) FROM entries WHERE id IN (1,2)"); got != originals {
		t.Fatal("original evidence changed")
	}
	if got := scalar(t, db, "SELECT json_group_array(json_array(id,parent_id,source_id,session_id,turn_id,file_tip_id)) FROM entries"); got != relations {
		t.Fatal("entry identities changed")
	}
	if got, err := os.ReadFile(archive); err != nil || string(got) != string(archiveBytes) {
		t.Fatal("archive changed", err)
	}
	if _, err := db.Exec(`UPDATE tool_calls SET result_json='{}' WHERE id='call'`); err == nil {
		t.Fatal("immutable tool result trigger removed")
	}
	before := scalar(t, db, "SELECT json_group_array(content_json) FROM entries")
	applySchema5(t, db)
	if got := scalar(t, db, "SELECT json_group_array(content_json) FROM entries"); got != before {
		t.Fatal("version 6 not idempotent")
	}
}

func TestCorruptionRollsBack(t *testing.T) {
	tests := map[string]string{
		"age":               `UPDATE entries SET content_json=json_set(content_json,'$.content',replace(json_extract(content_json,'$.content'),'"age_ms":100','"age_ms":99')) WHERE id=11`,
		"source":            `UPDATE entries SET content_json=json_set(content_json,'$.input_source','normal') WHERE id=12`,
		"time":              `UPDATE entries SET created_ms=99 WHERE id=1`,
		"text":              `UPDATE entries SET content_json=json_set(content_json,'$.content','different text') WHERE id=22`,
		"attachment":        `UPDATE entries SET content_json=json_remove(content_json,'$.files') WHERE id=12`,
		"orphan":            `DELETE FROM entries WHERE id=16; UPDATE entries SET parent_id=10 WHERE id=15`,
		"missing_original":  `UPDATE entries SET source_id=10 WHERE id=12`,
		"cycle":             `UPDATE entries SET source_id=22 WHERE id=1`,
		"cursor_provenance": `UPDATE entries SET content_json=json_set(content_json,'$.content',replace(json_extract(content_json,'$.content'),'"compaction_at_ms":200','"compaction_at_ms":201')) WHERE id=31`,
		"duplicate_key":     `UPDATE entries SET content_json=json_set(content_json,'$.content',replace(json_extract(content_json,'$.content'),'"age_ms":100','"age_ms":100,"age_ms":100')) WHERE id=11`,
		"unknown_key":       `UPDATE entries SET content_json=json_set(content_json,'$.content',replace(json_extract(content_json,'$.content'),'"age_ms":100','"age_ms":100,"extra":0')) WHERE id=11`,
		"case_key":          `UPDATE entries SET content_json=json_set(content_json,'$.content',replace(json_extract(content_json,'$.content'),'"age_ms"','"Age_MS"')) WHERE id=11`,
		"pair_identity":     `INSERT INTO entries(id,session_id,actor_id,kind,role,model_visible,content_json,created_ms) SELECT 3,session_id,actor_id,kind,role,model_visible,content_json,created_ms FROM entries WHERE id=1; UPDATE entries SET source_id=3 WHERE id=22`,
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			db := fixture(t)
			exec(t, db, corrupt)
			before := scalar(t, db, "SELECT json_group_array(content_json) FROM entries")
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			err = Apply(tx, nil)
			if err == nil {
				tx.Rollback()
				t.Fatal("accepted corruption")
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if got := scalar(t, db, "PRAGMA user_version"); got != "5" {
				t.Fatal(got)
			}
			if got := scalar(t, db, "SELECT json_group_array(content_json) FROM entries"); got != before {
				t.Fatal("partial rewrite")
			}
		})
	}
	// A write failure after earlier rewrites also rolls back through the caller.
	t.Run("write_failure", func(t *testing.T) {
		db := fixture(t)
		exec(t, db, `CREATE TRIGGER reject_migration BEFORE UPDATE ON entries WHEN OLD.id=22 BEGIN SELECT RAISE(ABORT,'write failure'); END`)
		before := scalar(t, db, "SELECT json_group_array(content_json) FROM entries")
		tx, _ := db.Begin()
		if err := Apply(tx, nil); err == nil {
			t.Fatal("write failure ignored")
		}
		tx.Rollback()
		if got := scalar(t, db, "SELECT json_group_array(content_json) FROM entries"); got != before {
			t.Fatal("partial rewrite")
		}
	})
}

func TestUserLookalikeAndOtherSources(t *testing.T) {
	for _, source := range []string{"normal", "task", "btw"} {
		t.Run(source, func(t *testing.T) {
			db := fixture(t)
			originalTime := int64(130)
			if source != "normal" {
				originalTime = 125
			}
			human, _ := json.Marshal(map[string]any{"role": "user", "content": "assigned exact text", "input_source": source, "input_time_ms": originalTime})
			insert(t, db, 40, "old", 2, 0, "", "message", "user", string(human), 130)
			insert(t, db, 41, "continued", 14, 0, "", "message", "developer", marker(t, source, originalTime, 230), 230)
			insert(t, db, 42, "continued", 41, 40, "", "message", "user", string(human), 230)
			if source != "normal" {
				exec(t, db, `UPDATE entries SET actor_id='child' WHERE id IN (40,41,42)`)
			}
			lookalike := strings.Replace(marker(t, "normal", 100, 200), `"role":"developer"`, `"role":"user"`, 1)
			insert(t, db, 43, "old", 2, 0, "", "message", "user", lookalike, 140)
			applySchema5(t, db)
			if got := scalar(t, db, "SELECT content_json FROM entries WHERE id=43"); got != lookalike {
				t.Fatal("user text interpreted as marker")
			}
			if got := scalar(t, db, "SELECT json_extract(content_json,'$.input_source') FROM entries WHERE id=42"); got != source {
				t.Fatal(got)
			}
		})
	}
}
func TestOriginalAdmissionFieldsAreEnrichedOnlyOnCopies(t *testing.T) {
	db := fixture(t)
	exec(t, db, `UPDATE entries SET content_json=json_set(content_json,'$.input_time_ms',1) WHERE id=1`)
	exec(t, db, `UPDATE entries SET content_json=json_set(content_json,'$.input_time_ms',1,'$.input_source','queue') WHERE id=2`)
	before := scalar(t, db, `SELECT json_group_array(content_json) FROM entries WHERE id IN (1,2)`)
	applySchema5(t, db)
	if got := scalar(t, db, `SELECT json_group_array(content_json) FROM entries WHERE id IN (1,2)`); got != before {
		t.Fatal("original admissions rewritten")
	}
	if got := scalar(t, db, `SELECT json_array(json_extract(content_json,'$.input_time_ms'),json_extract(content_json,'$.input_source')) FROM entries WHERE id=12`); got != `[100,"queue"]` {
		t.Fatal(got)
	}
	if got := scalar(t, db, `SELECT json_array(json_extract(content_json,'$.input_time_ms'),json_extract(content_json,'$.input_source')) FROM entries WHERE id=14`); got != `[110,"steer"]` {
		t.Fatal(got)
	}
}

func TestFreshAdmissionFollowingCopiedMarkerCutIsUntouched(t *testing.T) {
	for _, lookalike := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "marker_lookalike"}[lookalike], func(t *testing.T) {
			db := fixture(t)
			human := `{"role":"user","content":"new prompt","input_time_ms":1}`
			if lookalike {
				human = strings.Replace(marker(t, "queue", 100, 200), `"role":"developer"`, `"role":"user"`, 1)
			}
			insert(t, db, 40, "cut", 31, 0, "", "message", "user", human, 400)
			exec(t, db, `UPDATE sessions SET active_entry_id=40 WHERE id='cut'`)
			applySchema5(t, db)
			if got := scalar(t, db, `SELECT content_json FROM entries WHERE id=40`); got != human {
				t.Fatal("fresh admission rewritten", got)
			}
			if got := scalar(t, db, `SELECT json_array(active_entry_id,redo_entry_id,undo_floor_id) FROM sessions WHERE id='cut'`); got != `[40,30,30]` {
				t.Fatal(got)
			}
		})
	}
}

func replacementBranch(t *testing.T, db *sql.DB, parent int64) {
	t.Helper()
	if parent == 33 {
		insert(t, db, 33, "cut", 30, 31, "", "message", "developer", marker(t, "queue", 100, 200), 350)
	}
	human := `{"role":"user","content":"replacement prompt","files":[{"path":"/fresh.png","sha256":"fresh","bytes":9}],"input_time_ms":400,"input_source":"normal"}`
	session := map[int64]string{11: "continued", 31: "cut", 33: "cut"}[parent]
	insert(t, db, 40, session, parent, 0, "", "message", "user", human, 400)
	insert(t, db, 41, "copy", 20, parent, "", "message", "developer", marker(t, "queue", 100, 200), 500)
	insert(t, db, 42, "copy", 41, 40, "", "message", "user", human, 500)
	insert(t, db, 43, "cut", 30, 41, "", "message", "developer", marker(t, "queue", 100, 200), 600)
	insert(t, db, 44, "cut", 43, 42, "", "message", "user", human, 600)
}

func TestFreshReplacementAndCopiedDescendantsAreUntouched(t *testing.T) {
	for _, parent := range []int64{11, 31} {
		for _, active := range []bool{false, true} {
			name := map[int64]string{11: "original_marker", 31: "copied_marker"}[parent]
			name += map[bool]string{false: "/abandoned", true: "/active"}[active]
			t.Run(name, func(t *testing.T) {
				db := fixture(t)
				replacementBranch(t, db, parent)
				if active {
					session := map[int64]string{11: "continued", 31: "cut"}[parent]
					exec(t, db, `UPDATE sessions SET active_entry_id=40 WHERE id=?`, session)
				}
				unchanged := `SELECT json_group_array(json_array(id,parent_id,source_id,created_ms,content_json)) FROM entries WHERE id IN (1,2,40,42,44)`
				before := scalar(t, db, unchanged)
				applySchema5(t, db)
				if got := scalar(t, db, unchanged); got != before {
					t.Fatal("original admissions or fresh replacement descendants changed", got)
				}
				if got := scalar(t, db, `SELECT count(*) FROM entries WHERE id IN (12,14,16,22) AND json_extract(content_json,'$.retained_at_ms') IS NOT NULL`); got != "4" {
					t.Fatal("authentic retained inputs not migrated", got)
				}
				if got := scalar(t, db, `SELECT count(*) FROM entries WHERE id IN (41,43) AND kind='status' AND role IS NULL AND model_visible=0`); got != "2" {
					t.Fatal("copied replacement boundaries not migrated", got)
				}
			})
		}
	}
}

func retainReplacement(t *testing.T, db *sql.DB) {
	t.Helper()
	human := scalar(t, db, `SELECT content_json FROM entries WHERE id=40`)
	// A new compaction retains the twice-loaded replacement, then its retained
	// pair is loaded twice more. All copies resolve to the same human origin.
	insert(t, db, 50, "continued", 10, 0, "", "message", "developer", marker(t, "normal", 400, 700), 700)
	insert(t, db, 51, "continued", 50, 44, "", "message", "user", human, 700)
	insert(t, db, 52, "copy", 20, 50, "", "message", "developer", marker(t, "normal", 400, 700), 800)
	insert(t, db, 53, "copy", 52, 51, "", "message", "user", human, 800)
	insert(t, db, 54, "cut", 30, 52, "", "message", "developer", marker(t, "normal", 400, 700), 900)
	insert(t, db, 55, "cut", 54, 53, "", "message", "user", human, 900)
}

func TestFreshReplacementCanBeRetainedByLaterCompaction(t *testing.T) {
	for _, parent := range []int64{11, 31, 33} {
		t.Run(map[int64]string{11: "original_marker", 31: "copied_marker", 33: "second_load_marker"}[parent], func(t *testing.T) {
			db := fixture(t)
			replacementBranch(t, db, parent)
			retainReplacement(t, db)
			unchanged := `SELECT json_group_array(json_array(id,parent_id,source_id,created_ms,content_json)) FROM entries WHERE id IN (1,2,40,42,44)`
			before := scalar(t, db, unchanged)
			applySchema5(t, db)
			if got := scalar(t, db, unchanged); got != before {
				t.Fatal("fresh admissions or their old-lineage copies changed", got)
			}
			for _, id := range []int64{51, 53, 55} {
				if got := scalar(t, db, `SELECT json_array(json_extract(content_json,'$.input_time_ms'),json_extract(content_json,'$.input_source'),json_extract(content_json,'$.retained_at_ms')) FROM entries WHERE id=?`, id); got != `[400,"normal",700]` {
					t.Fatal("later retention metadata missing", id, got)
				}
				if got := scalar(t, db, `SELECT json_array(json_extract(content_json,'$.content'),json_extract(content_json,'$.files')) FROM entries WHERE id=?`, id); got != scalar(t, db, `SELECT json_array(json_extract(content_json,'$.content'),json_extract(content_json,'$.files')) FROM entries WHERE id=40`) {
					t.Fatal("later retained content changed", id, got)
				}
			}
			if got := scalar(t, db, `SELECT count(*) FROM entries WHERE id IN (50,52,54) AND kind='status' AND role IS NULL AND model_visible=0`); got != "3" {
				t.Fatal("later marker lineage not migrated", got)
			}
		})
	}
}

func TestLaterRetentionOfFreshReplacementRejectsCorruption(t *testing.T) {
	for name, corrupt := range map[string]string{
		"missing_evidence": `UPDATE entries SET parent_id=10 WHERE id=51`,
		"source":           `UPDATE entries SET content_json=json_set(content_json,'$.input_source','queue') WHERE id=51`,
		"content":          `UPDATE entries SET content_json=json_set(content_json,'$.content','corrupt') WHERE id=53`,
		"time":             `UPDATE entries SET content_json=json_set(content_json,'$.input_time_ms',401) WHERE id=55`,
		"marker_age":       `UPDATE entries SET content_json=json_set(content_json,'$.content',replace(json_extract(content_json,'$.content'),'"age_ms":300','"age_ms":299')) WHERE id=50`,
	} {
		t.Run(name, func(t *testing.T) {
			db := fixture(t)
			replacementBranch(t, db, 31)
			retainReplacement(t, db)
			exec(t, db, corrupt)
			before := scalar(t, db, `SELECT json_group_array(content_json) FROM entries`)
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(tx, nil); err == nil {
				tx.Rollback()
				t.Fatal("accepted corrupt later retention")
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if got := scalar(t, db, `SELECT json_group_array(content_json) FROM entries`); got != before {
				t.Fatal("partial rewrite")
			}
			if got := scalar(t, db, `PRAGMA user_version`); got != "5" {
				t.Fatal("schema advanced after failed migration", got)
			}
		})
	}
}

func TestFreshReplacementDoesNotHideCorruptRetainedEvidence(t *testing.T) {
	for name, corrupt := range map[string]string{
		"missing_evidence": `UPDATE entries SET parent_id=10 WHERE id=12`,
		"source":           `UPDATE entries SET content_json=json_set(content_json,'$.input_source','normal') WHERE id=12`,
		"content":          `UPDATE entries SET content_json=json_set(content_json,'$.content','corrupt') WHERE id=12`,
		"time":             `UPDATE entries SET created_ms=99 WHERE id=1`,
		"marker_age":       `UPDATE entries SET content_json=json_set(content_json,'$.content',replace(json_extract(content_json,'$.content'),'"age_ms":100','"age_ms":99')) WHERE id=11`,
		"runtime_child":    `UPDATE entries SET content_json=json_set(content_json,'$.runtime',json('true')) WHERE id=40`,
	} {
		t.Run(name, func(t *testing.T) {
			db := fixture(t)
			replacementBranch(t, db, 11)
			exec(t, db, corrupt)
			before := scalar(t, db, `SELECT json_group_array(content_json) FROM entries`)
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(tx, nil); err == nil {
				tx.Rollback()
				t.Fatal("fresh replacement hid corrupt retained evidence")
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if got := scalar(t, db, `SELECT json_group_array(content_json) FROM entries`); got != before {
				t.Fatal("partial rewrite")
			}
			if got := scalar(t, db, `PRAGMA user_version`); got != "5" {
				t.Fatal("schema advanced after failed migration", got)
			}
		})
	}
}

func TestInvisibleChildAdmissionsAndHistoricalRequestsAreUntouched(t *testing.T) {
	db := fixture(t)
	// Child compaction retains marker pairs only in isolated runtime memory;
	// canonical child admissions remain invisible, without stored marker pairs.
	human := `{"role":"user","content":"child assignment","input_source":"task","input_time_ms":125}`
	insert(t, db, 40, "old", 2, 0, "", "message", "user", human, 130)
	exec(t, db, `UPDATE entries SET actor_id='main/child',model_visible=0 WHERE id=40`)
	request := "[" + marker(t, "task", 125, 200) + "," + human + "]"
	exec(t, db, `INSERT INTO model_requests(id,session_id,actor_id,input_json,purpose,model_json,status,attempts_json,created_ms) VALUES(2,'old','main/child',?,'coding','{}','completed','[]',200)`, request)
	applySchema5(t, db)
	if got := scalar(t, db, `SELECT content_json FROM entries WHERE id=40`); got != human {
		t.Fatal("invisible child admission rewritten")
	}
	if got := scalar(t, db, `SELECT input_json FROM model_requests WHERE id=2`); got != request {
		t.Fatal("child request evidence rewritten")
	}
	if got := scalar(t, db, `SELECT count(*) FROM sqlite_temp_master WHERE name='retention_cuts'`); got != "0" {
		t.Fatal("temporary migration graph leaked")
	}
}

func TestUnsupportedVersions(t *testing.T) {
	for _, version := range []string{"-1", "0", "1", "2", "3", "4", "12", "999"} {
		t.Run(version, func(t *testing.T) {
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			exec(t, db, "PRAGMA user_version="+version)
			tx, _ := db.Begin()
			defer tx.Rollback()
			if err := Apply(tx, func(event Progress) { t.Fatalf("unsupported version reported progress: %+v", event) }); err == nil || !strings.Contains(err.Error(), "unsupported history schema version "+version) {
				t.Fatal(err)
			}
			var after string
			if err := tx.QueryRow("PRAGMA user_version").Scan(&after); err != nil || after != version {
				t.Fatalf("unsupported version changed: version=%q err=%v", after, err)
			}
		})
	}
}
