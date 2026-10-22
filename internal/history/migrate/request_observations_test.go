package migrate

import (
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func observationFixture(t *testing.T) *sql.DB {
	t.Helper()
	db := fixture(t)
	tx := transaction(t, db)
	for _, step := range upgrades {
		if step.from == 11 {
			break
		}
		if err := step.run(tx, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", step.from+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return db
}

func observationEvent(t *testing.T, db *sql.DB, id int, session, actor, raw string, source int) {
	t.Helper()
	exec(t, db, `INSERT INTO entries(id,session_id,source_id,actor_id,kind,model_visible,content_json,created_ms) VALUES(?,?,nullif(?,0),?,'status',0,?,1)`, id, session, source, actor, raw)
}

func TestSchema11ObservationsPreserveImmutableEvidence(t *testing.T) {
	db := observationFixture(t)
	checkpoint := `{"version":1,"local_input":0,"compatible":"` + strings.Repeat("a", 64) + `","exact":"` + strings.Repeat("b", 64) + `"}`
	input := ` { "messages": [], "input_estimate": ` + checkpoint + ` } `
	exec(t, db, `UPDATE model_requests SET input_json=?,status='failed',attempts_json='[{"status":"completed","usage":{"input_tokens":999}}]' WHERE id=1`, input)
	observationEvent(t, db, 100, "old", "main", ` { "type":"request_admitted", "request_id":1 } `, 0)
	// Original prompt may belong to a continuation, unlike the admission.
	observationEvent(t, db, 101, "continued", "main", `{"type":"system_prompt","request_id":1,"path":"/not-read"}`, 0)
	events := []string{
		`{"type":"request_finished","request_id":1,"status":"completed","attempts":[{"status":"completed","usage":{"input_tokens":9}},{"status":"failed","usage":{"input_tokens":99}},{"status":"completed","usage":{"input_tokens":0,"output_tokens":3,"cached_input_tokens":0}}]}`,
		`{"type":"request_finished","request_id":1,"status":"completed","attempts":[{"status":"completed"}]}`,
		`{"type":"request_finished","request_id":1,"status":"completed","attempts":[{"status":"completed","usage":null}]}`,
		`{"type":"request_finished","request_id":1,"status":"completed","attempts":[{"status":"completed","usage":{"output_tokens":5}}]}`,
		`{"type":"request_finished","request_id":1,"status":"failed","attempts":[{"status":"failed","usage":{"input_tokens":100}}]}`,
		`{"type":"request_finished","request_id":1,"status":"interrupted"}`,
	}
	for i, raw := range events {
		observationEvent(t, db, 102+i, "old", "main", raw, 0)
	}
	observationEvent(t, db, 120, "copy", "main", events[0], 102)
	observationEvent(t, db, 121, "copy", "main", `{"type":"request_admitted","request_id":1}`, 100)
	for i, purpose := range []string{"naming", "compaction", "coding"} {
		id := i + 2
		exec(t, db, `INSERT INTO model_requests(id,session_id,actor_id,purpose,model_json,status,attempts_json,created_ms) VALUES(?,'old','main/child',?,'{}','running','[]',1)`, id, purpose)
		observationEvent(t, db, 130+i, "old", "main/child", fmt.Sprintf(`{"type":"request_finished","request_id":%d,"status":"completed","attempts":[{"status":"completed","usage":{"input_tokens":8}}]}`, id), 0)
	}
	before := scalar(t, db, `SELECT json_group_array(json_array(id,content_json)) FROM entries`)
	requestsBefore := scalar(t, db, `SELECT json_group_array(json_array(id,input_json,status)) FROM model_requests`)
	archive := t.TempDir() + "/archive.jsonl"
	if err := os.WriteFile(archive, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	tx := transaction(t, db)
	var progress []Progress
	if err := Apply(tx, func(p Progress) { progress = append(progress, p) }); err != nil {
		t.Fatal(err)
	}
	checkProgress(t, progress, 11)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, db, `SELECT json_group_array(json_array(id,content_json)) FROM entries`); got != before {
		t.Fatal("entry bytes changed")
	}
	if got := scalar(t, db, `SELECT json_group_array(json_array(id,input_json,status)) FROM model_requests`); got != requestsBefore {
		t.Fatal("request bytes changed")
	}
	if got, err := os.ReadFile(archive); err != nil || string(got) != input {
		t.Fatal("archive changed", err)
	}
	if got := scalar(t, db, `SELECT count(*) FROM request_observations`); got != "9" {
		t.Fatal(got)
	}
	if got := scalar(t, db, `SELECT json_array(admission_entry_id,system_prompt_entry_id) FROM model_requests WHERE id=1`); got != "[100,101]" {
		t.Fatal(got)
	}
	if got := scalar(t, db, `SELECT json_array(admission_entry_id,system_prompt_entry_id,status,json_extract(usage_json,'$.input_tokens'),json_extract(usage_json,'$.output_tokens'),json_extract(usage_json,'$.cached_input_tokens'),json_extract(checkpoint_json,'$.local_input')) FROM request_observations WHERE completion_entry_id=102`); got != `[100,101,"completed",0,3,0,0]` {
		t.Fatal(got)
	}
	if got := scalar(t, db, `SELECT count(*) FROM request_observations WHERE completion_entry_id BETWEEN 103 AND 107 AND usage_json IS NULL`); got != "5" {
		t.Fatal(got)
	}
	if got := scalar(t, db, `SELECT count(*) FROM request_observations WHERE completion_entry_id IN (120,121)`); got != "0" {
		t.Fatal("copied duplicate", got)
	}
	if got := scalar(t, db, `SELECT count(*) FROM pragma_table_info('model_requests') WHERE name='attempts_json'`); got != "0" {
		t.Fatal("obsolete mutable attempts column retained", got)
	}
}

func TestSchema11OrphanAttemptDiagnosticsRollback(t *testing.T) {
	for _, copied := range []bool{false, true} {
		t.Run(fmt.Sprintf("copied_completion=%t", copied), func(t *testing.T) {
			db := observationFixture(t)
			exec(t, db, `UPDATE model_requests SET attempts_json='[{"status":"failed","error":"only remaining diagnostic"}]' WHERE id=1`)
			if copied {
				observationEvent(t, db, 100, "copy", "main", `{"type":"request_finished","request_id":1,"status":"failed"}`, 1)
			}
			before := databaseEvidence(t, db)
			tx := transaction(t, db)
			err := Apply(tx, nil)
			if err == nil || !strings.Contains(err.Error(), "request 1: mutable attempt diagnostics have no original request_finished event") {
				t.Fatal("orphan diagnostics were silently deleted", err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			after := databaseEvidence(t, db)
			delete(before, "changes")
			delete(after, "changes")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("orphan diagnostics failure changed database evidence")
			}
			if got := scalar(t, db, "PRAGMA user_version"); got != "11" {
				t.Fatal(got)
			}
		})
	}
}

func TestSchema11EmptyAttemptDiagnosticsNeedNoCompletion(t *testing.T) {
	for _, attempts := range []string{"[]", " [ ] ", "null"} {
		t.Run(attempts, func(t *testing.T) {
			db := observationFixture(t)
			exec(t, db, `UPDATE model_requests SET attempts_json=?`, attempts)
			applyCurrent(t, db)
			if got := scalar(t, db, `SELECT count(*) FROM pragma_table_info('model_requests') WHERE name='attempts_json'`); got != "0" {
				t.Fatal("obsolete mutable attempts column retained", got)
			}
			if got := scalar(t, db, `SELECT count(*) FROM request_observations`); got != "0" {
				t.Fatal("fabricated missing completion", got)
			}
		})
	}
}

func TestSchema11MalformedEvidenceRollsBack(t *testing.T) {
	for _, test := range []struct{ name, raw, input string }{
		{"usage_type", `{"status":"completed","attempts":[{"status":"completed","usage":"bad"}]}`, ""},
		{"input_type", `{"status":"completed","attempts":[{"status":"completed","usage":{"input_tokens":true}}]}`, ""},
		{"output_type", `{"status":"completed","attempts":[{"status":"completed","usage":{"input_tokens":0,"output_tokens":"invalid"}}]}`, ""},
		{"attempts_type", `{"status":"completed","attempts":{}}`, ""},
		{"status", `{"status":"running"}`, ""},
		{"checkpoint", `{"status":"completed"}`, `{"input_estimate":{"version":2}}`},
		{"checkpoint_type", `{"status":"completed"}`, `{"input_estimate":"null"}`},
		{"usage_null_string", `{"status":"completed","attempts":[{"status":"completed","usage":"null"}]}`, ""},
		{"missing_request", `{"status":"completed","request_id":999}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := observationFixture(t)
			if test.input != "" {
				exec(t, db, `UPDATE model_requests SET input_json=? WHERE id=1`, test.input)
			}
			raw := `{"type":"request_finished","request_id":1,` + test.raw[1:]
			if test.name == "missing_request" {
				raw = `{"type":"request_finished","status":"completed","request_id":999}`
			}
			observationEvent(t, db, 100, "old", "main", raw, 0)
			before := databaseEvidence(t, db)
			tx := transaction(t, db)
			var progress []Progress
			err := Apply(tx, func(p Progress) { progress = append(progress, p) })
			if err == nil || !strings.Contains(err.Error(), "schema 11 to 12") {
				t.Fatal("missing migration error", err)
			}
			for _, p := range progress {
				if p.Phase == "complete" {
					t.Fatal("failed migration claimed completion")
				}
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			// total_changes counts rolled-back writes; all durable data/layout must match.
			after := databaseEvidence(t, db)
			delete(before, "changes")
			delete(after, "changes")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rollback changed database evidence")
			}
			if got := scalar(t, db, "PRAGMA user_version"); got != "11" {
				t.Fatal(got)
			}
		})
	}
}

func TestSchema11NegativeUsageUnavailableWithoutChangingAudit(t *testing.T) {
	db := observationFixture(t)
	for i, purpose := range []string{"coding", "naming", "compaction", "coding"} {
		actor := "main"
		if i == 3 {
			actor = "main/child"
		}
		request := i + 2
		exec(t, db, `INSERT INTO model_requests(id,session_id,actor_id,purpose,model_json,status,attempts_json,created_ms) VALUES(?,'old',?,?,'{}','completed','[]',1)`, request, actor, purpose)
		for j, usage := range []string{`{"input_tokens":-1,"output_tokens":2}`, `{"input_tokens":0,"output_tokens":-1}`, `{"input_tokens":0,"output_tokens":0}`} {
			raw := fmt.Sprintf(` { "type":"request_finished", "request_id":%d, "status":"completed", "attempts":[{"status":"completed","usage":%s}] } `, request, usage)
			observationEvent(t, db, 100+3*i+j, "old", actor, raw, 0)
		}
	}
	entriesBefore := scalar(t, db, `SELECT json_group_array(json_array(id,content_json)) FROM entries`)
	requestsBefore := scalar(t, db, `SELECT json_group_array(json_array(id,input_json,status)) FROM model_requests`)
	applyCurrent(t, db)
	if got := scalar(t, db, `SELECT count(*) FROM request_observations WHERE status='completed' AND usage_json IS NULL`); got != "8" {
		t.Fatal("negative usage was not unavailable", got)
	}
	if got := scalar(t, db, `SELECT count(*) FROM request_observations WHERE json_extract(usage_json,'$.input_tokens')=0 AND json_extract(usage_json,'$.output_tokens')=0`); got != "4" {
		t.Fatal("measured zero was lost", got)
	}
	if got := scalar(t, db, `SELECT json_group_array(json_array(id,content_json)) FROM entries`); got != entriesBefore {
		t.Fatal("negative usage audit bytes changed")
	}
	if got := scalar(t, db, `SELECT json_group_array(json_array(id,input_json,status)) FROM model_requests`); got != requestsBefore {
		t.Fatal("negative usage request audit changed")
	}
}

func TestSchema12FreshMigrationParity(t *testing.T) {
	migrated := observationFixture(t)
	applyCurrent(t, migrated)
	fresh, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	schema, err := os.ReadFile("../schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(t, fresh, string(schema))
	// Column ordering differs after ALTER TABLE; compare each named column's type,
	// nullability and primary-key constraints, plus all foreign keys and indexes.
	for _, query := range []string{
		`SELECT json_group_array(json_array(name,type,"notnull",dflt_value,pk)) FROM (SELECT * FROM pragma_table_info('model_requests') ORDER BY name)`,
		`SELECT json_group_array(json_array(name,type,"notnull",dflt_value,pk)) FROM (SELECT * FROM pragma_table_info('request_observations') ORDER BY name)`,
		`SELECT json_group_array(json_array("table","from","to",on_update,on_delete)) FROM (SELECT * FROM pragma_foreign_key_list('model_requests') ORDER BY "from")`,
		`SELECT json_group_array(json_array("table","from","to",on_update,on_delete)) FROM (SELECT * FROM pragma_foreign_key_list('request_observations') ORDER BY "from")`,
		`SELECT json_group_array(json_array(name,"unique",partial)) FROM (SELECT * FROM pragma_index_list('request_observations') ORDER BY name)`,
		`SELECT json_group_array(name) FROM pragma_index_info('request_observation_request')`,
		`PRAGMA user_version`,
	} {
		if a, b := scalar(t, migrated, query), scalar(t, fresh, query); a != b {
			t.Fatalf("schema mismatch: %s\nmigrated=%s fresh=%s", query, a, b)
		}
	}
}

func TestSchema11ProgressBatchesAndSuccessfulRollback(t *testing.T) {
	db := observationFixture(t)
	for i := 0; i < 2*progressBatchSize+3; i++ {
		observationEvent(t, db, 100+i, "old", "main", `{"type":"request_finished","request_id":1,"status":"completed","attempts":[{"status":"completed","usage":{"input_tokens":0}}]}`, 0)
	}
	// Unrelated status bodies must not enter the accounting decoder or counts.
	observationEvent(t, db, 500, "old", "main", `{"type":"other","attempts":"not accounting evidence","body":"`+strings.Repeat("x", 1<<20)+`"}`, 0)
	before := databaseEvidence(t, db)
	tx := transaction(t, db)
	var events []Progress
	if err := Apply(tx, func(p Progress) { events = append(events, p) }); err != nil {
		t.Fatal(err)
	}
	checkProgress(t, events, 11)
	readBatch, writeBatch := false, false
	for _, p := range events {
		readBatch = readBatch || p.Phase == "read_request_evidence" && p.Completed >= progressBatchSize
		writeBatch = writeBatch || p.Phase == "write_request_observations" && p.Completed > 2*progressBatchSize+5
	}
	if !readBatch || !writeBatch {
		t.Fatalf("missing batched progress: %+v", events)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	after := databaseEvidence(t, db)
	delete(before, "changes")
	delete(after, "changes")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("successful Apply committed changes without caller commit")
	}
}

func TestSchema11CompletionSnapshotsPromptAtEachCut(t *testing.T) {
	db := observationFixture(t)
	observationEvent(t, db, 100, "old", "main", `{"type":"system_prompt","request_id":0}`, 0)
	observationEvent(t, db, 101, "old", "main", `{"type":"request_finished","request_id":1,"status":"interrupted"}`, 0)
	observationEvent(t, db, 102, "old", "main", `{"type":"request_admitted","request_id":1}`, 0)
	observationEvent(t, db, 103, "continued", "main", `{"type":"system_prompt","request_id":1}`, 0)
	observationEvent(t, db, 104, "continued", "main", `{"type":"request_finished","request_id":1,"status":"completed","attempts":[{"status":"completed","usage":{"input_tokens":1}}]}`, 0)
	observationEvent(t, db, 105, "continued", "main", `{"type":"system_prompt","request_id":1}`, 0)
	observationEvent(t, db, 106, "continued", "main", `{"type":"request_finished","request_id":1,"status":"failed","attempts":[{"status":"completed","usage":{"input_tokens":2}}]}`, 0)
	applyCurrent(t, db)
	if got := scalar(t, db, `SELECT json_group_array(json_array(completion_entry_id,admission_entry_id,system_prompt_entry_id,status,usage_json IS NULL)) FROM (SELECT * FROM request_observations ORDER BY completion_entry_id)`); got != `[[101,null,null,"interrupted",1],[104,102,103,"completed",0],[106,102,105,"failed",1]]` {
		t.Fatal("completion ownership leaked across cuts", got)
	}
	if got := scalar(t, db, `SELECT json_array(admission_entry_id,system_prompt_entry_id) FROM model_requests WHERE id=1`); got != `[102,105]` {
		t.Fatal("request did not retain latest prompt", got)
	}
}
