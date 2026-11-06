package migrate

import (
	"fmt"
	"strings"
	"testing"
)

func TestSchema14ChildSteerReceiptsPreserveEvidence(t *testing.T) {
	db := fixture(t)
	applyCurrent(t, db)
	// Schema 15 changes payload names only; its table layout is schema 14's.
	exec(t, db, "PRAGMA user_version=14")
	const delivered = `{"type":"runtime_event","body":{"type":"child_message_delivered","child_id":"main/child","child_turn_id":"turn","job_id":"job","message_event_seq":9007199254740993,"request_id":1,"note":"child_message_delivered","extra":{"large":123456789012345678901234567890,"decimal":1.234567890123456789e100}}}`
	const undelivered = `{"type":"runtime_event","recovered_from_event_seq":9007199254740997,"body":{"type":"child_message_undelivered","child_id":"main/child","child_turn_id":"turn","job_id":"job","message_event_seq":9007199254740995}}`
	for i, raw := range []string{delivered, undelivered, delivered, undelivered} {
		id := 100 + i
		exec(t, db, `INSERT INTO entries(id,session_id,parent_id,source_id,delivered_request_id,actor_id,kind,model_visible,content_json,created_ms)
			VALUES(?,'old',2,CASE WHEN ?>=102 THEN 100 ELSE NULL END,CASE WHEN ?=100 THEN 1 ELSE NULL END,'main','status',0,?,123456789)`, id, id, id, raw)
	}
	// Model messages, provider replay state and request-message snapshots are
	// exact evidence, not canonical event envelopes. Even event-shaped payloads
	// outside status records must not be rewritten.
	const message = ` { "type":"runtime_notification", "event_seq":100, "content":"{\"type\":\"child_message_delivered\"}", "state":{"provider":"fixture","model":"fixture","version":1,"items":[{"type":"child_message_delivered","opaque":9007199254740993}]} } `
	insert(t, db, 110, "old", 2, 0, "", "message", "user", message, 200)
	insert(t, db, 111, "old", 2, 0, "", "tool_result", "tool", delivered, 200)
	insert(t, db, 112, "old", 2, 0, "", "message", "user", delivered, 200)
	for id, raw := range map[int]string{
		113: ` { "type":"request_message", "request_id":1, "role":"user", "message":` + message + ` } `,
		114: ` { "type":"runtime_event", "body":{"type":"child_message","message":"child_message_undelivered"} } `,
		115: ` { "type":"runtime_event", "body":{"type":"child_steer_delivered","message_event_seq":100} } `,
		116: ` { "type":"child_message_delivered", "body":{"type":"unrelated"} } `,
	} {
		exec(t, db, `INSERT INTO entries(id,session_id,actor_id,kind,model_visible,content_json,created_ms) VALUES(?,'old','main','status',0,?,200)`, id, raw)
	}
	exec(t, db, `UPDATE model_requests SET input_json=? WHERE id=1`, "["+message+"]")
	exec(t, db, `INSERT INTO tool_calls VALUES('receipt_evidence','old',1,'receipt_provider','main','fixture',1,?,?)`, delivered, undelivered)
	// Keep semantic IDs, copied provenance, delivery acknowledgments, ownership,
	// timestamps and all unrelated byte strings unchanged.
	const evidence = `SELECT json_array(
		(SELECT json_group_array(json_array(id,content_json)) FROM entries WHERE id NOT BETWEEN 100 AND 103),
		(SELECT json_group_array(json_array(id,parent_id,source_id,session_id,turn_id,delivered_request_id,main_turn_id,undo_owner_turn_id,actor_id,kind,role,model_visible,file_tip_id,created_ms)) FROM entries),
		(SELECT json_group_array(json_array(id,input_json,delivered_events_json,model_json,status,event_cutoff,admission_entry_id,system_prompt_entry_id)) FROM model_requests),
		(SELECT json_group_array(json_array(id,call_json,result_json)) FROM tool_calls),
		(SELECT json_group_array(json_array(id,active_entry_id,redo_entry_id,undo_floor_id,file_tip_id,metadata_json)) FROM sessions))`
	before := scalar(t, db, evidence)
	var events []Progress
	tx := transaction(t, db)
	if err := Apply(tx, func(event Progress) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	checkProgress(t, events, 14)
	for i, raw := range []string{delivered, undelivered, delivered, undelivered} {
		old, renamed := "child_message_delivered", "child_steer_delivered"
		if i%2 == 1 {
			old, renamed = "child_message_undelivered", "child_steer_undelivered"
		}
		want := strings.Replace(raw, `"type":"`+old+`"`, `"type":"`+renamed+`"`, 1)
		if got := scalar(t, db, "SELECT content_json FROM entries WHERE id=?", 100+i); got != want {
			t.Fatalf("receipt %d: got %s, want %s", i, got, want)
		}
	}
	if got := scalar(t, db, evidence); got != before {
		t.Fatal("migration changed saved model/provider evidence, guidance or event identity")
	}
	if got := scalar(t, db, "SELECT count(*) FROM pragma_foreign_key_check"); got != "0" {
		t.Fatal("migration damaged foreign keys", got)
	}
	if got := scalar(t, db, "PRAGMA user_version"); got != fmt.Sprint(CurrentVersion) {
		t.Fatal("migration did not advance schema", got)
	}
	before = scalar(t, db, "SELECT json_group_array(json_array(id,content_json)) FROM entries")
	applyCurrent(t, db)
	if got := scalar(t, db, "SELECT json_group_array(json_array(id,content_json)) FROM entries"); got != before {
		t.Fatal("migration is not idempotent")
	}
}

func TestSchema14ChildSteerReceiptFailureRollsBack(t *testing.T) {
	db := fixture(t)
	applyCurrent(t, db)
	exec(t, db, "PRAGMA user_version=14")
	for _, id := range []int{100, 101} {
		exec(t, db, `INSERT INTO entries(id,session_id,actor_id,kind,model_visible,content_json,created_ms)
			VALUES(?,'old','main','status',0,'{"type":"runtime_event","body":{"type":"child_message_undelivered"}}',200)`, id)
	}
	exec(t, db, `CREATE TRIGGER reject_receipt_migration BEFORE UPDATE ON entries WHEN OLD.id=101
		BEGIN SELECT RAISE(ABORT,'injected receipt migration failure'); END`)
	before := scalar(t, db, "SELECT json_group_array(json_array(id,content_json)) FROM entries")
	tx := transaction(t, db)
	var events []Progress
	err := Apply(tx, func(event Progress) { events = append(events, event) })
	if err == nil || !strings.Contains(err.Error(), "migrate history schema 14 to 15") || !strings.Contains(err.Error(), "injected receipt migration failure") {
		t.Fatal("missing migration failure", err)
	}
	for _, event := range events {
		if event.Phase == "complete" {
			t.Fatal("failed migration reported completion")
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, db, "SELECT json_group_array(json_array(id,content_json)) FROM entries"); got != before {
		t.Fatal("failed migration partially rewrote history")
	}
	if got := scalar(t, db, "PRAGMA user_version"); got != "14" {
		t.Fatal("failed migration changed schema version", got)
	}
}
