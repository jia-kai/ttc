package history

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"scicode/internal/provider"
	"scicode/internal/render"
)

func TestRequestAdmissionCutoffInputAndRollback(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	source, err := s.Append(v.ID, turn, "main", "status", "", false, map[string]any{"type": "job_exit", "job_id": "done"})
	if err != nil {
		t.Fatal(err)
	}
	notice := provider.Message{Role: "user", Runtime: true, Content: `{"type":"job_exit","job_id":"done"}`, EventSeq: source}
	cm := &provider.Message{Role: "developer", Runtime: true, Content: `{"type":"runtime_context"}`}
	before, err := s.Session(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	var countBefore int
	if err := s.DB.QueryRow("SELECT count(*) FROM model_requests").Scan(&countBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_admission BEFORE INSERT ON model_requests BEGIN SELECT RAISE(ABORT,'forced admission failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitRequest(v.ID, turn, "main", v.Model, cm, []provider.Message{notice}, nil); err == nil {
		t.Fatal("forced failure succeeded")
	}
	after, err := s.Session(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	var countAfter int
	if err := s.DB.QueryRow("SELECT count(*) FROM model_requests").Scan(&countAfter); err != nil {
		t.Fatal(err)
	}
	if before.EntryTip != after.EntryTip || countBefore != countAfter {
		t.Fatal("admission rollback changed committed history", before, after, countBefore, countAfter)
	}
	if _, err := s.DB.Exec("DROP TRIGGER fail_admission"); err != nil {
		t.Fatal(err)
	}
	admitted, err := s.AdmitRequest(v.ID, turn, "main", v.Model, cm, []provider.Message{notice}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.Cutoff != source || admitted.ContextEntry <= admitted.Cutoff || len(admitted.NoticeEntries) != 1 || admitted.NoticeEntries[0] <= admitted.Cutoff {
		t.Fatal(admitted)
	}
	var input, ack string
	var cutoff int64
	if err := s.DB.QueryRow("SELECT event_cutoff,input_json,delivered_events_json FROM model_requests WHERE id=?", admitted.RequestID).Scan(&cutoff, &input, &ack); err != nil {
		t.Fatal(err)
	}
	var metadata requestInputMetadata
	var delivered []int64
	if err := json.Unmarshal([]byte(input), &metadata); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(ack), &delivered); err != nil {
		t.Fatal(err)
	}
	if cutoff != source || metadata.Version != 1 || metadata.MessageCount != len(admitted.Messages) || len(input) > 128 || !reflect.DeepEqual(delivered, []int64{source}) {
		t.Fatal(cutoff, metadata, delivered)
	}
	messages := admitted.Messages
	last := messages[len(messages)-1]
	if last.RequestID != admitted.RequestID || messages[len(messages)-2].EventSeq != source {
		t.Fatal(messages)
	}
	// Events committed after admission cannot mutate its metadata or frozen input.
	if _, err := s.Append(v.ID, turn, "main", "status", "", false, map[string]any{"type": "later"}); err != nil {
		t.Fatal(err)
	}
	var reread string
	if err := s.DB.QueryRow("SELECT input_json FROM model_requests WHERE id=?", admitted.RequestID).Scan(&reread); err != nil || reread != input {
		t.Fatal(reread, err)
	}
}

func TestRequestInputMetadataStaysBoundedForMainAndChildReplay(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	item := json.RawMessage(`{"type":"reasoning","encrypted_content":"` + strings.Repeat("A", 1<<20) + `"}`)
	reply := provider.Message{Role: "assistant", State: &provider.ReplayState{Provider: v.Model.Provider, Model: v.Model.Model.RequestID(), Version: 1, Items: []json.RawMessage{item}}}
	if _, err := s.Append(v.ID, turn, "main", "message", "assistant", true, reply); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"main", "main/child_metadata"} {
		var prefix []provider.Message
		actorTurn := turn
		if actor != "main" {
			prefix = []provider.Message{{Role: "user", Content: "Frozen parent context"}, reply}
			var err error
			actorTurn, err = s.BeginChildTurn(v.ID, actor, v.Model)
			if err != nil {
				t.Fatal(err)
			}
		}
		admitted, err := s.AdmitRequest(v.ID, actorTurn, actor, v.Model, nil, nil, prefix)
		if err != nil {
			t.Fatal(actor, err)
		}
		last := admitted.Messages[len(admitted.Messages)-1]
		if last.State == nil || len(last.State.Items) != 1 || string(last.State.Items[0]) != string(item) {
			t.Fatal("bounded metadata changed replay", actor)
		}
		var input string
		if err := s.DB.QueryRow("SELECT input_json FROM model_requests WHERE id=?", admitted.RequestID).Scan(&input); err != nil {
			t.Fatal(err)
		}
		var metadata requestInputMetadata
		if err := json.Unmarshal([]byte(input), &metadata); err != nil || metadata.Version != 1 || metadata.MessageCount != len(admitted.Messages) || len(input) > 128 {
			t.Fatal("request copied full input", actor, len(input), metadata, err)
		}
	}
}

func TestSemanticCopiesAndChildUndoOwnership(t *testing.T) {
	s, v, mainTurn, _ := historyFixture(t)
	childTurn, err := s.BeginChildTurn(v.ID, "main/child", v.Model)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Append(v.ID, childTurn, "main/child", "status", "", false, map[string]any{"type": "child_finished"})
	if err != nil {
		t.Fatal(err)
	}
	original, err := s.Entry(id)
	if err != nil {
		t.Fatal(err)
	}
	if original.MainTurnID != mainTurn || original.UndoOwnerTurnID != mainTurn || original.TurnID != childTurn {
		t.Fatal(original)
	}
	copyID := id
	for range 2 {
		entry, err := s.Entry(copyID)
		if err != nil {
			t.Fatal(err)
		}
		err = s.transact(func(tx *sql.Tx) error {
			var err error
			copyID, err = appendTx(tx, v.ID, childTurn, entry.Actor, entry.Kind, entry.Role, entry.Visible, entry.Content, entry.EventSeq())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		copy, err := s.Entry(copyID)
		if err != nil || copy.ID == id || copy.EventSeq() != id || copy.MainTurnID != original.MainTurnID || copy.UndoOwnerTurnID != original.UndoOwnerTurnID {
			t.Fatal(copy, err)
		}
	}
	if err := s.FinishTurn(mainTurn, "completed"); err != nil {
		t.Fatal(err)
	}
	async, err := s.BeginTurn(v.ID, "async", v.Model)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.Append(v.ID, async, "main", "status", "", false, map[string]any{"type": "async"})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Entry(latest)
	if err != nil || entry.MainTurnID != async || entry.UndoOwnerTurnID != mainTurn {
		t.Fatal(entry, err)
	}
}

func TestRejectedFutureNotificationRollsBackAndChildInputStaysIsolated(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	before, err := s.Session(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	cm := &provider.Message{Role: "developer", Runtime: true, Content: `{"type":"runtime_context"}`}
	_, err = s.AdmitRequest(v.ID, turn, "main", v.Model, cm, []provider.Message{{Role: "user", Content: "future", EventSeq: before.EntryTip + 100}}, nil)
	if err == nil || !strings.Contains(err.Error(), "cutoff") {
		t.Fatal(err)
	}
	after, err := s.Session(v.ID)
	if err != nil || before.EntryTip != after.EntryTip {
		t.Fatal(after, err)
	}
	childTurn, err := s.BeginChildTurn(v.ID, "main/child", v.Model)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []provider.Message{{Role: "user", Content: "frozen child prefix"}}
	admitted, err := s.AdmitRequest(v.ID, childTurn, "main/child", v.Model, cm, nil, prefix)
	if err != nil || len(admitted.Messages) != 2 || admitted.Messages[0].Content != "frozen child prefix" {
		t.Fatal(admitted, err)
	}
	main, err := s.Messages(v.ID)
	if err != nil || len(main) != 1 || main[0].Content != "hello" {
		t.Fatal(main, err)
	}
}

func TestForegroundFinishAcknowledgmentIsOnceOnly(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	finish, err := s.Append(v.ID, turn, "main/child", "status", "", false, map[string]string{"type": "child_turn_finished"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.StartRequest(v.ID, turn, "main", "coding", v.Model)
	if err != nil {
		t.Fatal(err)
	}
	call, err := s.CallIntent(v.ID, turn, "main", req, provider.ToolCall{ID: "child", Name: "subagent", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"ok": true, "finish_event_seq": finish})
	if _, err = s.CallResult(v.ID, turn, "main", call, result, map[string]string{}, render.Markdown{}, true); err != nil {
		t.Fatal(err)
	}
	cm := &provider.Message{Role: "developer", Runtime: true, Content: `{"type":"runtime_context"}`}
	// A fast background assignment can be represented by both its immutable
	// launch result and the queued finish; it still receives one acknowledgment.
	first, err := s.AdmitRequest(v.ID, turn, "main", v.Model, cm, []provider.Message{{Role: "user", Runtime: true, Content: `{"type":"child_turn_finished"}`, EventSeq: finish}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AdmitRequest(v.ID, turn, "main", v.Model, cm, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id   int64
		want string
	}{{first.RequestID, fmt.Sprintf("[%d]", finish)}, {second.RequestID, "[]"}} {
		var ack string
		if err := s.DB.QueryRow("SELECT delivered_events_json FROM model_requests WHERE id=?", test.id).Scan(&ack); err != nil || ack != test.want {
			t.Fatal(ack, test.want, err)
		}
	}
}
