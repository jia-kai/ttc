package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestRequestMessagesAttributionOrderAndIsolation(t *testing.T) {
	for _, actor := range []string{"main", "main/child_batch", "main/aside_batch"} {
		t.Run(actor, func(t *testing.T) {
			s, session, turn, _ := historyFixture(t)
			source, err := s.Append(session.ID, turn, "main", "status", "", false, map[string]any{"type": "runtime_event", "body": map[string]string{"type": "job_exit"}})
			if err != nil {
				t.Fatal(err)
			}
			request, err := s.StartRequest(session.ID, turn, actor, "compaction", session.Model)
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.Messages(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			branch, err := s.Branch(session.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			var requestBefore string
			const requestState = "SELECT json_array(event_cutoff,input_json,delivered_events_json,status) FROM model_requests WHERE id=?"
			if err := s.db.QueryRow(requestState, request).Scan(&requestBefore); err != nil {
				t.Fatal(err)
			}
			messages := []llm.Message{
				{Type: llm.TypeSystemInstruction, Content: "system"},
				{Type: llm.TypeDeveloperInstruction, Content: "developer"},
				{Type: llm.TypeUserInput, Content: "user", Files: []llm.BinaryFile{{Path: "fixture.pdf", MIMEType: "application/pdf", Bytes: 4}}},
				{Type: llm.TypeAssistant, Content: "assistant", Calls: []llm.ToolCall{{ID: "call", Name: "read", Arguments: json.RawMessage(`{"path":"fixture"}`)}}, State: &llm.ReplayState{Provider: "fixture", Model: "fixture", Version: 1, Items: []json.RawMessage{json.RawMessage(`{"opaque":"state"}`)}}},
				{Type: llm.TypeToolResult, Content: "result", CallID: "call"},
				{Type: llm.TypeRuntimeContext, Content: `{"type":"runtime_context"}`, RequestID: request},
				{Type: llm.TypeRuntimeNotification, Content: `{"type":"job_exit"}`, EventSeq: source},
				{Type: llm.TypeChildGuidance, Content: "guidance", EventSeq: source},
				{Type: llm.TypeRuntimeInstruction, Content: "summary directive"},
			}
			ids, err := s.RequestMessages(session.ID, turn, "compaction", request, messages)
			if err != nil || len(ids) != len(messages) {
				t.Fatalf("RequestMessages = %v, %v", ids, err)
			}
			parent := branch[len(branch)-1].ID
			for i, id := range ids {
				entry, err := s.Entry(id)
				if err != nil {
					t.Fatal(err)
				}
				if entry.Parent != parent || entry.SessionID != session.ID || entry.TurnID != turn || entry.Actor != actor || entry.Kind != "status" || entry.Role != "" || entry.Visible || entry.Source != 0 {
					t.Fatalf("entry %d attribution/order/visibility: %+v", i, entry)
				}
				want, err := json.Marshal(map[string]any{"type": "request_message", "purpose": "compaction", "request_id": request, "role": messages[i].Role(), "message": messages[i]})
				if err != nil || string(entry.Content) != string(want) {
					t.Fatalf("entry %d payload = %s, want %s (%v)", i, entry.Content, want, err)
				}
				var payload struct {
					Message llm.Message `json:"message"`
				}
				if err := json.Unmarshal(entry.Content, &payload); err != nil || !reflect.DeepEqual(payload.Message, messages[i]) {
					t.Fatalf("canonical message %d changed: %+v, %v", i, payload.Message, err)
				}
				view, err := s.ReadEntryView(context.Background(), id)
				if err != nil || view.StatusType != "request_message" || view.Message != nil || view.Label != "compaction "+messages[i].Role()+" · inspect" {
					t.Fatalf("entry %d placeholder: %+v, %v", i, view, err)
				}
				parent = id
			}
			after, err := s.Messages(session.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("batch entered coding context: before=%+v after=%+v err=%v", before, after, err)
			}
			var delivered sql.NullInt64
			if err := s.db.QueryRow("SELECT delivered_request_id FROM entries WHERE id=?", source).Scan(&delivered); err != nil || delivered.Valid {
				t.Fatalf("batch acknowledged runtime event: %v, %v", delivered, err)
			}
			var requestAfter string
			if err := s.db.QueryRow(requestState, request).Scan(&requestAfter); err != nil || requestBefore != requestAfter {
				t.Fatalf("batch changed request admission: %s -> %s, %v", requestBefore, requestAfter, err)
			}
		})
	}
}

func TestRequestMessagesRollback(t *testing.T) {
	for _, failure := range []string{"invalid type", "incompatible fields", "marshal", "storage", "read only", "session", "turn", "purpose", "missing request"} {
		t.Run(failure, func(t *testing.T) {
			s, session, turn, request := historyFixture(t)
			messages := []llm.Message{{Type: llm.TypeUserInput, Content: "first"}, {Type: llm.TypeAssistant, Content: "second"}}
			sessionID, batchTurn, purpose := session.ID, turn, "coding"
			switch failure {
			case "invalid type":
				messages[1].Type = "unknown"
			case "incompatible fields":
				messages[1].CallID = "tool-only"
			case "marshal":
				messages[1].Calls = []llm.ToolCall{{Arguments: json.RawMessage("invalid JSON")}}
			case "storage":
				if _, err := s.db.Exec(`CREATE TRIGGER fail_second_request_message BEFORE INSERT ON entries
					WHEN json_extract(NEW.content_json,'$.message.content')='second'
					BEGIN SELECT RAISE(ABORT,'forced second message failure'); END`); err != nil {
					t.Fatal(err)
				}
			case "read only":
				if _, err := s.db.Exec("UPDATE sessions SET read_only=1 WHERE id=?", session.ID); err != nil {
					t.Fatal(err)
				}
			case "session":
				sessionID = "another-session"
			case "turn":
				batchTurn = "another-turn"
			case "purpose":
				purpose = "compaction"
			case "missing request":
				request = -1
			}
			before, err := s.Session(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			branchBefore, err := s.Branch(session.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			var countBefore int
			if err := s.db.QueryRow("SELECT count(*) FROM entries").Scan(&countBefore); err != nil {
				t.Fatal(err)
			}
			ids, err := s.RequestMessages(sessionID, batchTurn, purpose, request, messages)
			if err == nil || ids != nil {
				t.Fatalf("failed batch returned IDs=%v error=%v", ids, err)
			}
			if failure == "storage" && !strings.Contains(err.Error(), "forced second message failure") {
				t.Fatalf("did not reach second insertion: %v", err)
			}
			after, err := s.Session(session.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rollback changed session: before=%+v after=%+v err=%v", before, after, err)
			}
			branchAfter, err := s.Branch(session.ID, 0)
			if err != nil || !reflect.DeepEqual(branchBefore, branchAfter) {
				t.Fatalf("rollback changed branch: %v", err)
			}
			var countAfter int
			if err := s.db.QueryRow("SELECT count(*) FROM entries").Scan(&countAfter); err != nil || countBefore != countAfter {
				t.Fatalf("rollback left entries: %d -> %d, %v", countBefore, countAfter, err)
			}
		})
	}
}

func TestRequestMessagesEmptyBatchAndNullTurn(t *testing.T) {
	s, session, _, _ := historyFixture(t)
	request, err := s.StartRequest(session.ID, "", "main", "naming", session.Model)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Session(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := s.RequestMessages(session.ID, "", "naming", request, nil)
	if err != nil || len(ids) != 0 {
		t.Fatalf("empty batch = %v, %v", ids, err)
	}
	after, err := s.Session(session.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("empty batch changed session: %+v -> %+v, %v", before, after, err)
	}
	ids, err = s.RequestMessages(session.ID, "", "naming", request, []llm.Message{{Type: llm.TypeUserInput, Content: "name this"}})
	if err != nil || len(ids) != 1 {
		t.Fatalf("null-turn batch = %v, %v", ids, err)
	}
}

func TestRequestMessagesFollowCompactionContinuation(t *testing.T) {
	s, original, _, _ := historyFixture(t)
	actor := "main/child_inflight"
	turn, err := s.BeginChildTurn(original.ID, actor, original.Model)
	if err != nil {
		t.Fatal(err)
	}
	request, err := s.StartRequest(original.ID, turn, actor, "compaction", original.Model)
	if err != nil {
		t.Fatal(err)
	}
	current := original
	for range 2 {
		cut, err := s.Session(current.ID)
		if err != nil {
			t.Fatal(err)
		}
		current, err = s.Continue(current.ID, llm.NewCompactionContext("handoff"), cut.EntryTip, cut.EntryTip, nil, time.Now(), nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	frozen, err := s.Branch(original.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	messages := []llm.Message{{Type: llm.TypeRuntimeInstruction, Content: "summary input"}, {Type: llm.TypeAssistant, Content: "summary reply"}}
	ids, err := s.RequestMessages(current.ID, turn, "compaction", request, messages)
	if err != nil || len(ids) != len(messages) {
		t.Fatalf("continuation batch = %v, %v", ids, err)
	}
	for _, id := range ids {
		entry, err := s.Entry(id)
		if err != nil || entry.SessionID != current.ID || entry.TurnID != turn || entry.Actor != actor {
			t.Fatalf("continuation lost attribution: %+v, %v", entry, err)
		}
	}
	var owner string
	if err := s.db.QueryRow("SELECT session_id FROM model_requests WHERE id=?", request).Scan(&owner); err != nil || owner != original.ID {
		t.Fatalf("request ownership changed: %q, %v", owner, err)
	}
	if after, err := s.Branch(original.ID, 0); err != nil || !reflect.DeepEqual(frozen, after) {
		t.Fatalf("continuation modified predecessor: %v", err)
	}
	loaded, err := s.Load(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Session(loaded.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids, err = s.RequestMessages(loaded.ID, turn, "compaction", request, messages)
	if err == nil || ids != nil {
		t.Fatalf("cross-lineage batch accepted: %v, %v", ids, err)
	}
	if after, err := s.Session(loaded.ID); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("cross-lineage rejection modified target: %+v, %v", after, err)
	}
}
