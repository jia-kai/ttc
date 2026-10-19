package migrate

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func applyCurrent(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Apply(tx, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSchema6MessageTypesPreserveEvidenceAndNestedStatus(t *testing.T) {
	db := fixture(t)
	applySchema5(t, db)
	exec(t, db, "PRAGMA foreign_keys=ON")
	request := scalar(t, db, "SELECT input_json FROM model_requests WHERE id=1")
	relations := scalar(t, db, "SELECT json_group_array(json_array(id,parent_id,source_id,session_id,turn_id,file_tip_id)) FROM entries")
	cases := []struct {
		role    string
		runtime bool
		content string
		typ     llm.MessageType
	}{
		{"user", false, `{"type":"runtime_context","message_type":"runtime_notification"}`, llm.TypeUserInput},
		{"assistant", false, "answer", llm.TypeAssistant},
		{"tool", false, "result", llm.TypeToolResult},
		{"developer", false, "instructions", llm.TypeDeveloperInstruction},
		{"developer", true, `{"type":"runtime_context"}`, llm.TypeRuntimeContext},
		{"developer", true, "retry continuation", llm.TypeRuntimeInstruction},
		{"user", true, `{"type":"job_exit","job_id":"j"}`, llm.TypeRuntimeNotification},
		{"user", true, `{"type":"child_message","message":"guidance"}`, llm.TypeChildGuidance},
	}
	for i, tc := range cases {
		raw, _ := json.Marshal(map[string]any{"role": tc.role, "runtime": tc.runtime, "content": tc.content, "event_seq": int64(10 + i)})
		insert(t, db, int64(100+i), "old", 2, 0, "", "message", tc.role, string(raw), 130)
	}
	nested := `{"type":"request_message","purpose":"naming","request_id":1,"role":"assistant","message":{"role":"assistant","content":"title","phase":"final"}}`
	exec(t, db, `INSERT INTO entries(id,session_id,parent_id,actor_id,kind,model_visible,content_json,created_ms) VALUES(150,'old',2,'main','status',0,?,140)`, nested)
	// More than two batches catches keyset termination/skipping and unbounded
	// history buffering regressions through complete record preservation.
	for i := 0; i < 2*messageTypeBatchSize+3; i++ {
		insert(t, db, int64(200+i), "old", 2, 0, "", "message", "assistant", fmt.Sprintf(`{"role":"assistant","content":"batch-%d"}`, i), 150)
	}
	exec(t, db, `CREATE TRIGGER semantic_test_trigger BEFORE DELETE ON entries WHEN OLD.id=1 BEGIN SELECT RAISE(ABORT,'preserved'); END`)
	exec(t, db, `UPDATE sqlite_sequence SET seq=900 WHERE name='entries'`)
	applyCurrent(t, db)
	if got := scalar(t, db, "PRAGMA user_version"); got != "10" {
		t.Fatal(got)
	}
	for i, tc := range cases {
		raw := scalar(t, db, "SELECT content_json FROM entries WHERE id=?", 100+i)
		var m llm.Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if m.Type != tc.typ || m.Role() != tc.role || m.Content != tc.content || m.EventSeq != int64(10+i) {
			t.Fatal("changed semantics/provenance", m, tc)
		}
		if strings.Contains(raw, `"runtime":`) || strings.Contains(raw, `"role":`) {
			t.Fatal("legacy classification survived", raw)
		}
	}
	var status struct {
		Type, Purpose, Role string
		RequestID           int64 `json:"request_id"`
		Message             llm.Message
	}
	if err := json.Unmarshal([]byte(scalar(t, db, "SELECT content_json FROM entries WHERE id=150")), &status); err != nil {
		t.Fatal(err)
	}
	if status.Type != "request_message" || status.Purpose != "naming" || status.Role != "assistant" || status.RequestID != 1 || status.Message.Type != llm.TypeAssistant || status.Message.Content != "title" || status.Message.Phase != "final" {
		t.Fatal("nested message damaged its enclosing status", status)
	}
	for i := 0; i < 2*messageTypeBatchSize+3; i++ {
		if got := scalar(t, db, "SELECT json_extract(content_json,'$.type')||':'||json_extract(content_json,'$.content') FROM entries WHERE id=?", 200+i); got != fmt.Sprintf("assistant:batch-%d", i) {
			t.Fatal("batch lost record", i, got)
		}
	}
	if got := scalar(t, db, "SELECT input_json FROM model_requests WHERE id=1"); got != request {
		t.Fatal("saved request rewritten")
	}
	if got := scalar(t, db, "SELECT json_group_array(json_array(id,parent_id,source_id,session_id,turn_id,file_tip_id)) FROM entries WHERE id<100"); got != relations {
		t.Fatal("entry identity changed")
	}
	if got := scalar(t, db, "SELECT count(*) FROM pragma_foreign_key_check"); got != "0" {
		t.Fatal("foreign keys damaged", got)
	}
	if got := scalar(t, db, "SELECT count(*) FROM sqlite_schema WHERE name IN ('entry_session','entry_parent','entry_source','semantic_test_trigger')"); got != "4" {
		t.Fatal("entry index/trigger lost", got)
	}
	// The indexed role supports system instructions on both fresh and upgraded DBs.
	insert(t, db, 500, "old", 2, 0, "", "message", "system", `{"type":"system_instruction","content":"system"}`, 160)
	if got := scalar(t, db, "SELECT seq FROM sqlite_sequence WHERE name='entries'"); got != "900" {
		t.Fatal("event identity high-water mark lost", got)
	}
	before := scalar(t, db, "SELECT json_group_array(content_json) FROM entries")
	applyCurrent(t, db)
	if got := scalar(t, db, "SELECT json_group_array(content_json) FROM entries"); got != before {
		t.Fatal("migration not idempotent")
	}
}

func TestSchema6CorruptClassificationRollsBack(t *testing.T) {
	for _, raw := range []string{`{"role":"unknown"}`, `{"role":"assistant","runtime":true}`, `{"role":"user","runtime":"true"}`, `{"role":"assistant","content":"wrong indexed role"}`} {
		t.Run(raw, func(t *testing.T) {
			db := fixture(t)
			applySchema5(t, db)
			insert(t, db, 100, "old", 2, 0, "", "message", "user", raw, 130)
			before := scalar(t, db, "SELECT json_group_array(content_json) FROM entries")
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(tx, nil); err == nil {
				tx.Rollback()
				t.Fatal("accepted invalid legacy classification")
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if got := scalar(t, db, "PRAGMA user_version"); got != "6" {
				t.Fatal(got)
			}
			if got := scalar(t, db, "SELECT json_group_array(content_json) FROM entries"); got != before {
				t.Fatal("partially migrated corrupt history")
			}
		})
	}
}

func TestMessageTypeBatchBoundsBytesAndAllowsOversizedEntry(t *testing.T) {
	db := fixture(t)
	applySchema5(t, db)
	lengths := []int{3 << 20, 3 << 20, 3 << 20, 3 << 20, 9 << 20}
	for i, size := range lengths {
		raw, err := json.Marshal(map[string]string{"role": "assistant", "content": strings.Repeat("x", size)})
		if err != nil {
			t.Fatal(err)
		}
		insert(t, db, int64(100+i), "old", 2, 0, "", "message", "assistant", string(raw), 150)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	last, seen := int64(99), 0
	var batchSizes []int
	for {
		updates, err := messageTypeBatch(tx, last)
		if err != nil {
			t.Fatal(err)
		}
		if len(updates) == 0 {
			break
		}
		batchSizes = append(batchSizes, len(updates))
		var bytes int
		for _, update := range updates {
			if update.id != int64(100+seen) {
				t.Fatal("byte boundary skipped an entry", update.id, seen)
			}
			var message llm.Message
			if err := json.Unmarshal(update.body, &message); err != nil {
				t.Fatal(err)
			}
			if message.Type != llm.TypeAssistant || len(message.Content) != lengths[seen] || strings.Trim(message.Content, "x") != "" {
				t.Fatal("large payload changed", update.id)
			}
			bytes += len(update.body)
			seen++
		}
		if bytes > messageTypeBatchBytes && len(updates) != 1 {
			t.Fatal("batch exceeded byte limit with multiple entries", bytes)
		}
		last = updates[len(updates)-1].id
	}
	if seen != len(lengths) || fmt.Sprint(batchSizes) != "[2 2 1]" {
		t.Fatal("incorrect byte-bounded batches", seen, batchSizes)
	}
}
