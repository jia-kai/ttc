package migrate

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resetSchema7(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, column := range []string{"archive_path", "archive_sha256", "archive_exact_sha256"} {
		exec(t, db, "ALTER TABLE compactions ADD COLUMN "+column+" TEXT NOT NULL DEFAULT ''")
	}
	exec(t, db, "DROP INDEX compaction_summary")
	exec(t, db, "PRAGMA user_version=7")
}

func schema7Fixture(t *testing.T) *sql.DB {
	t.Helper()
	db := fixture(t)
	applyCurrent(t, db)
	resetSchema7(t, db)
	return db
}

func completionInsert(t *testing.T, db *sql.DB, id int64, raw string) {
	t.Helper()
	exec(t, db, `INSERT INTO entries(id,session_id,parent_id,source_id,actor_id,kind,model_visible,content_json,created_ms) VALUES(?,'old',2,1,'main/child','status',0,?,123456789)`, id, raw)
}

func TestSchema7CompletionFormsAndEvidence(t *testing.T) {
	db := schema7Fixture(t)
	var ids []int64
	for _, nested := range []bool{false, true} {
		for _, spelling := range []string{"status", "assignment_status", "outcome"} {
			for _, outcome := range []string{"completed", "failed", "cancelled"} {
				for _, state := range []string{"", "idle", "closed", "unknown"} {
					if spelling == "outcome" && state == "" {
						continue
					}
					if state == "idle" && outcome != "completed" {
						continue // Idle is only valid for successful retained children.
					}
					stateField := ""
					if state != "" {
						key := "child_state"
						if spelling == "outcome" {
							key = "context_state"
						}
						stateField = fmt.Sprintf(`,%q:%q`, key, state)
					}
					if state == "idle" {
						stateField += `,"persistent":true`
					}
					body := fmt.Sprintf(`{"type":"child_turn_finished",%q:%q%s,"child_id":"child","child_turn_id":"turn","job_id":"job","result_entry_id":9007199254740993,"recovered_from_event_seq":9007199254740995,"answer":"exact answer","warning":"exact warning","truncated":true,"extra":{"large":123456789012345678901234567890,"decimal":1.234567890123456789e100}}`, spelling, outcome, stateField)
					if nested {
						body = `{"type":"runtime_event","recovered_from_event_seq":9007199254740997,"extra":9007199254740999,"body":` + body + `}`
					}
					id := int64(9007199254740993 + len(ids))
					ids = append(ids, id)
					completionInsert(t, db, id, body)
					applyCurrent(t, db)
					got := scalar(t, db, "SELECT content_json FROM entries WHERE id=?", id)
					var outer map[string]json.RawMessage
					if err := json.Unmarshal([]byte(got), &outer); err != nil {
						t.Fatal(err)
					}
					m := outer
					if nested {
						m = nil
						if err := json.Unmarshal(outer["body"], &m); err != nil {
							t.Fatal(err)
						}
						if string(outer["extra"]) != "9007199254740999" || string(outer["recovered_from_event_seq"]) != "9007199254740997" {
							t.Fatal("outer evidence changed", got)
						}
					}
					wantState := state
					if wantState == "" {
						wantState = "unknown"
					}
					if text(m, "outcome") != outcome || text(m, "context_state") != wantState || m["status"] != nil || m["assignment_status"] != nil || m["child_state"] != nil {
						t.Fatal("incorrect canonical completion", got)
					}
					if string(m["result_entry_id"]) != "9007199254740993" || string(m["recovered_from_event_seq"]) != "9007199254740995" || string(m["extra"]) != `{"large":123456789012345678901234567890,"decimal":1.234567890123456789e100}` || text(m, "answer") != "exact answer" || text(m, "warning") != "exact warning" || string(m["truncated"]) != "true" {
						t.Fatal("completion evidence changed", got)
					}
					if spelling == "outcome" && got != body {
						t.Fatal("canonical fixture was serialized again")
					}
					resetSchema7(t, db)
				}
			}
		}
	}
	// Saved provider/model/tool bytes and unrelated canonical entry bodies must
	// never be decoded as completion payloads, even when they look like one.
	legacy := ` { "type":"child_turn_finished", "status":"completed", "extra":9007199254740993 } `
	insert(t, db, 100, "old", 2, 0, "", "message", "user", legacy, 200)
	insert(t, db, 101, "old", 2, 0, "", "tool_result", "tool", legacy, 200)
	completionInsert(t, db, 102, ` { "type":"runtime_event", "body":{"type":"other","status":"completed"} } `)
	completionInsert(t, db, 103, ` { "type":"request_message", "message":`+legacy+` } `)
	exec(t, db, `INSERT INTO model_requests(id,session_id,actor_id,input_json,purpose,model_json,status,attempts_json,created_ms) VALUES(2,'old','main',?,'coding','{}','completed','[]',100)`, "["+legacy+"]")
	evidenceQuery := `SELECT json_array((SELECT json_group_array(json_array(id,content_json)) FROM entries WHERE id<1000),(SELECT json_group_array(json_array(id,input_json,attempts_json)) FROM model_requests),(SELECT json_group_array(json_array(id,result_json)) FROM tool_calls),(SELECT json_group_array(json_array(id,active_entry_id,redo_entry_id,undo_floor_id,file_tip_id)) FROM sessions),(SELECT json_group_array(json_array(id,parent_id,source_id,turn_id,delivered_request_id,created_ms)) FROM entries))`
	before := scalar(t, db, evidenceQuery)
	archive := filepath.Join(t.TempDir(), "archive.jsonl")
	if err := os.WriteFile(archive, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	applyCurrent(t, db)
	if got := scalar(t, db, evidenceQuery); got != before {
		t.Fatal("unrelated evidence, delivery or cursors changed")
	}
	if got, err := os.ReadFile(archive); err != nil || string(got) != legacy {
		t.Fatal("archive changed", err)
	}
	before = scalar(t, db, "SELECT json_group_array(json_array(id,content_json)) FROM entries")
	var seq int
	var name, path string
	if err := db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	applyCurrent(t, reopened)
	if got := scalar(t, reopened, "SELECT json_group_array(json_array(id,content_json)) FROM entries"); got != before {
		t.Fatal("reopen changed entries")
	}
	if got := scalar(t, reopened, "PRAGMA user_version"); got != "9" {
		t.Fatal(got)
	}
}

func TestSchema7CompletionPersistenceAndObservedState(t *testing.T) {
	for _, spelling := range []string{"status", "assignment_status", "outcome"} {
		for _, outcome := range []string{"completed", "failed", "cancelled"} {
			for _, state := range []string{"", "idle", "closed", "unknown"} {
				if spelling == "outcome" && state == "" {
					continue
				}
				for _, persistent := range []string{"", "false", "true"} {
					fields := fmt.Sprintf(`%q:%q`, spelling, outcome)
					if state != "" {
						key := "child_state"
						if spelling == "outcome" {
							key = "context_state"
						}
						fields += fmt.Sprintf(`,%q:%q`, key, state)
					}
					if persistent != "" {
						fields += `,"persistent":` + persistent
					}
					for _, nested := range []bool{false, true} {
						raw := `{"type":"child_turn_finished",` + fields + `}`
						if nested {
							raw = `{"type":"runtime_event","body":` + raw + `}`
						}
						got, err := canonicalChildCompletion([]byte(raw))
						invalid := state == "idle" && (outcome != "completed" || persistent != "true")
						if invalid {
							if err == nil {
								t.Fatalf("invalid idle completion accepted: %s", raw)
							}
							continue
						}
						if err != nil {
							t.Fatalf("valid completion rejected: %s: %v", raw, err)
						}
						body, err := completionObject(got)
						if err != nil {
							t.Fatal(err)
						}
						if nested {
							body, err = completionObject(body["body"])
							if err != nil {
								t.Fatal(err)
							}
						}
						wantState := state
						if wantState == "" {
							wantState = "unknown"
						}
						if text(body, "context_state") != wantState || string(body["persistent"]) != persistent {
							t.Fatalf("state inferred or persistence changed: %s => %s", raw, got)
						}
					}
				}
			}
		}
	}
}

func TestSchema7InvalidCompletionRollback(t *testing.T) {
	cases := []string{
		``, `"status":"running"`, `"status":1`, `"status":null`, `"status":true`, `"status":{}`,
		`"status":"completed","assignment_status":"completed"`,
		`"status":"completed","outcome":"completed"`,
		`"assignment_status":"failed","outcome":"completed"`,
		`"status":"completed","child_state":"idle","context_state":"idle"`,
		`"status":"completed","context_state":"idle"`,
		`"outcome":"completed","child_state":"idle"`,
		`"outcome":"completed"`, `"outcome":"completed","context_state":null`,
		`"status":"completed","child_state":"running"`,
		`"status":"completed","child_state":1`, `"status":"completed","child_state":null`,
		`"status":"completed","status":"failed"`,
		`"status":"completed","child_state":"idle","child_state":"closed"`,
		`"outcome":"failed","context_state":"\u0069dle","persistent":true`,
	}
	for _, spelling := range []string{"status", "assignment_status", "outcome"} {
		stateKey := "child_state"
		if spelling == "outcome" {
			stateKey = "context_state"
		}
		for _, outcome := range []string{"completed", "failed", "cancelled"} {
			for _, persistent := range []string{"", "false", "true"} {
				if outcome == "completed" && persistent == "true" {
					continue
				}
				fields := fmt.Sprintf(`%q:%q,%q:"idle"`, spelling, outcome, stateKey)
				if persistent != "" {
					fields += `,"persistent":` + persistent
				}
				cases = append(cases, fields)
			}
		}
		for _, state := range []string{"idle", "closed", "unknown"} {
			for _, persistent := range []string{"null", `"true"`, "0", "[]", "{}"} {
				cases = append(cases, fmt.Sprintf(`%q:"completed",%q:%q,"persistent":%s`, spelling, stateKey, state, persistent))
			}
		}
	}
	for i, fields := range cases {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/nested=%v", i, nested), func(t *testing.T) {
				db := schema7Fixture(t)
				for j := 0; j < messageTypeBatchSize+1; j++ {
					completionInsert(t, db, int64(100+j), `{"type":"child_turn_finished","status":"completed"}`)
				}
				raw := `{"type":"child_turn_finished"`
				if fields != "" {
					raw += "," + fields
				}
				raw += "}"
				if nested {
					raw = `{"type":"runtime_event","body":` + raw + `}`
				}
				completionInsert(t, db, 1000, raw)
				before := scalar(t, db, "SELECT json_group_array(content_json) FROM entries")
				tx := transaction(t, db)
				var events []Progress
				err := Apply(tx, func(p Progress) { events = append(events, p) })
				if err == nil || !strings.Contains(err.Error(), "migrate history schema 7 to 8: entry 1000") {
					t.Fatal("invalid completion accepted or lacks entry context", err)
				}
				for _, event := range events {
					if event.Phase == "complete" {
						t.Fatal("failed migration reported completion")
					}
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				if got := scalar(t, db, "PRAGMA user_version"); got != "7" {
					t.Fatal("failed migration updated version", got)
				}
				if got := scalar(t, db, "SELECT json_group_array(content_json) FROM entries"); got != before {
					t.Fatal("partial batch migration survived rollback")
				}
			})
		}
	}
}

func TestSchema7ProgressAndBoundedBatches(t *testing.T) {
	db := schema7Fixture(t)
	const count = 2*messageTypeBatchSize + 3
	for i := 0; i < count; i++ {
		completionInsert(t, db, int64(100+i), `{"type":"child_turn_finished","assignment_status":"completed","child_state":"closed"}`)
	}
	completionInsert(t, db, 500, `{"type":"other"}`)
	tx := transaction(t, db)
	batch, err := childCompletionBatch(tx, 0)
	if err != nil || len(batch) != messageTypeBatchSize {
		t.Fatal("row batch bound", len(batch), err)
	}
	var events []Progress
	if err := Apply(tx, func(p Progress) { events = append(events, p) }); err != nil {
		t.Fatal(err)
	}
	checkProgress(t, events, 7)
	if len(events) != 6 || events[0].Phase != "convert_child_completions" || events[0].Total != count+1 || events[1].Completed != messageTypeBatchSize || events[2].Completed != 2*messageTypeBatchSize {
		t.Fatalf("incorrect completion progress: %+v", events)
	}
	if got := scalarTx(t, tx, "SELECT count(*) FROM entries WHERE json_extract(content_json,'$.outcome')='completed'"); got != fmt.Sprint(count) {
		t.Fatal("batch skipped entries", got)
	}
}

func scalarTx(t *testing.T, tx *sql.Tx, query string) string {
	t.Helper()
	var value string
	if err := tx.QueryRow(query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestSchema7PayloadByteBatchBound(t *testing.T) {
	db := schema7Fixture(t)
	raw := `{"type":"child_turn_finished","status":"completed","extra":"` + strings.Repeat("x", messageTypeBatchBytes/2) + `"}`
	completionInsert(t, db, 100, raw)
	completionInsert(t, db, 101, raw)
	tx := transaction(t, db)
	batch, err := childCompletionBatch(tx, 0)
	if err != nil || len(batch) != 1 || batch[0].id != 100 {
		t.Fatal("byte batch bound", len(batch), err)
	}
	batch, err = childCompletionBatch(tx, 100)
	if err != nil || len(batch) != 1 || batch[0].id != 101 {
		t.Fatal("deferred row lost", len(batch), err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	oversized := `{"type":"child_turn_finished","status":"completed","extra":"` + strings.Repeat("x", messageTypeBatchBytes+1) + `"}`
	completionInsert(t, db, 102, oversized)
	tx = transaction(t, db)
	batch, err = childCompletionBatch(tx, 101)
	if err != nil || len(batch) != 1 || batch[0].id != 102 || len(batch[0].body) <= messageTypeBatchBytes {
		t.Fatal("oversized entry failed to make progress", len(batch), err)
	}
}

func TestSchema7ChildSnapshotForms(t *testing.T) {
	for _, stateKey := range []string{"", "state", "child_state", "context_state"} {
		for _, outcomeKey := range []string{"", "assignment_status", "outcome"} {
			for _, state := range []string{"running", "idle", "closed", "unknown"} {
				for _, outcome := range []string{"running", "completed", "failed", "cancelled", "unknown"} {
					child := `{"child_id":"child","label":"exact label","child_turn_id":"turn","job_id":"job","extra":9007199254740993}`
					var body map[string]json.RawMessage
					if err := json.Unmarshal([]byte(child), &body); err != nil {
						t.Fatal(err)
					}
					if stateKey != "" {
						body[stateKey], _ = json.Marshal(state)
					}
					if outcomeKey != "" {
						body[outcomeKey], _ = json.Marshal(outcome)
					}
					encoded, _ := json.Marshal(body)
					raw := ` { "type":"child_state", "extra":9007199254740995, "child":` + string(encoded) + ` } `
					got, err := canonicalChildCompletion([]byte(raw))
					if err != nil {
						t.Fatal(err, raw)
					}
					var outer map[string]json.RawMessage
					if err := json.Unmarshal(got, &outer); err != nil {
						t.Fatal(err)
					}
					body = nil
					if err := json.Unmarshal(outer["child"], &body); err != nil {
						t.Fatal(err)
					}
					wantState, wantOutcome := state, outcome
					if stateKey == "" {
						wantState = "unknown"
					}
					if outcomeKey == "" {
						wantOutcome = "unknown"
					}
					if text(body, "context_state") != wantState || text(body, "outcome") != wantOutcome || body["state"] != nil || body["child_state"] != nil || body["assignment_status"] != nil || text(body, "label") != "exact label" || text(body, "child_turn_id") != "turn" || text(body, "job_id") != "job" || string(body["extra"]) != "9007199254740993" || string(outer["extra"]) != "9007199254740995" {
						t.Fatal("snapshot semantics or evidence changed", string(got))
					}
					if stateKey == "context_state" && outcomeKey == "outcome" && string(got) != raw {
						t.Fatal("canonical snapshot serialized again")
					}
				}
			}
		}
	}
}

func TestSchema7ChildSnapshotSelectionAndRollback(t *testing.T) {
	db := schema7Fixture(t)
	completionInsert(t, db, 100, `{"type":"child_state","child":{"state":"idle","label":"saved"}}`)
	completionInsert(t, db, 101, `{"type":"child_state","child":{"child_state":"closed","assignment_status":"cancelled"}}`)
	exec(t, db, "UPDATE entries SET delivered_request_id=1 WHERE id IN (100,101)")
	message := ` { "type":"child_state", "child":{"state":"idle"} } `
	insert(t, db, 102, "old", 2, 0, "", "message", "user", message, 200)
	applyCurrent(t, db)
	if got := scalar(t, db, `SELECT json_extract(content_json,'$.child.context_state')||':'||json_extract(content_json,'$.child.outcome') FROM entries WHERE id=100`); got != "idle:unknown" {
		t.Fatal(got)
	}
	if got := scalar(t, db, `SELECT json_extract(content_json,'$.child.context_state')||':'||json_extract(content_json,'$.child.outcome') FROM entries WHERE id=101`); got != "closed:cancelled" {
		t.Fatal(got)
	}
	if got := scalar(t, db, "SELECT content_json FROM entries WHERE id=102"); got != message {
		t.Fatal("model body changed", got)
	}
	if got := scalar(t, db, "SELECT count(*) FROM entries WHERE id IN (100,101) AND delivered_request_id=1 AND source_id=1 AND parent_id=2 AND created_ms=123456789"); got != "2" {
		t.Fatal("snapshot identity/delivery changed", got)
	}
	for _, child := range []string{
		`null`, `[]`, `"idle"`,
		`{"state":"idle","child_state":"idle"}`,
		`{"state":"idle","context_state":"closed"}`,
		`{"child_state":"idle","context_state":"idle"}`,
		`{"assignment_status":"running","outcome":"running"}`,
		`{"state":null}`, `{"state":false}`, `{"state":"invalid"}`,
		`{"outcome":null}`, `{"assignment_status":1}`, `{"outcome":"idle"}`,
		`{"state":"idle","state":"closed"}`,
	} {
		t.Run(child, func(t *testing.T) {
			db := schema7Fixture(t)
			completionInsert(t, db, 100, `{"type":"child_state","child":`+child+`}`)
			tx := transaction(t, db)
			if err := Apply(tx, nil); err == nil {
				t.Fatal("invalid child snapshot accepted", child)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if got := scalar(t, db, "PRAGMA user_version"); got != "7" {
				t.Fatal(got)
			}
		})
	}
}

func TestSchema7FailureRollsBackEarlierUpgrades(t *testing.T) {
	db := fixture(t)
	completionInsert(t, db, 100, `{"type":"child_turn_finished","status":"invalid"}`)
	before := scalar(t, db, "SELECT json_group_array(json_array(id,content_json,kind,role,parent_id)) FROM entries")
	tx := transaction(t, db)
	if err := Apply(tx, nil); err == nil || !strings.Contains(err.Error(), "schema 7 to 8") {
		t.Fatal("expected final-step failure", err)
	}
	if got := scalarTx(t, tx, "PRAGMA user_version"); got != "7" {
		t.Fatal("earlier upgrades did not run", got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, db, "PRAGMA user_version"); got != "5" {
		t.Fatal("earlier version was committed", got)
	}
	if got := scalar(t, db, "SELECT json_group_array(json_array(id,content_json,kind,role,parent_id)) FROM entries"); got != before {
		t.Fatal("earlier upgrade changes survived rollback")
	}
}
