package history

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
	"ttc/internal/render"
)

func TestContinueLeavesMainCompactionMessageAuditInPredecessor(t *testing.T) {
	s, original, turn, coding := historyFixture(t)
	native := json.RawMessage(`{"type":"reasoning","encrypted_content":"original-native-prefix"}`)
	prefix := llm.Message{Type: llm.TypeAssistant, Content: "original assistant", State: &llm.ReplayState{
		Provider: "script", Model: original.Model.Model.ID, Version: 1, Items: []json.RawMessage{native},
	}}
	sourceTip, err := s.Append(original.ID, turn, "main", "message", "assistant", true, prefix)
	if err != nil {
		t.Fatal(err)
	}
	mainRequest, err := s.StartRequest(original.ID, turn, "main", "compaction", original.Model)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSystemPrompt(original.ID, turn, "main", mainRequest, "exact summary instructions"); err != nil {
		t.Fatal(err)
	}
	input := []llm.Message{
		{Type: llm.TypeUserInput, Content: "hello"}, prefix,
		{Type: llm.TypeRuntimeInstruction, Content: "summarize original structured prefix"},
		{Type: llm.TypeAssistant, Content: "summary audit reply", State: &llm.ReplayState{
			Provider: "script", Model: original.Model.Model.ID, Version: 1,
			Items: []json.RawMessage{json.RawMessage(`{"opaque":"summary-native-reply"}`)},
		}},
	}
	auditIDs, err := s.RequestMessages(original.ID, turn, "compaction", mainRequest, input)
	if err != nil {
		t.Fatal(err)
	}
	appendStatus := func(actor string, value any) int64 {
		t.Helper()
		id, err := s.Append(original.ID, turn, actor, "status", "", false, value)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Small lifecycle records remain available for chronology and recovery.
	appendStatus("main", map[string]any{"type": "request_retry", "request_id": mainRequest, "attempt": 2})
	if err := s.FinishRequest(mainRequest, RequestCompletion{Status: "completed", Attempts: []RequestAttempt{{Attempt: 2}}}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []struct{ actor, purpose string }{
		{"main/child_tail", "compaction"}, {"main/aside_tail", "compaction"},
		{"main", "coding"}, {"main", "naming"},
	} {
		request, err := s.StartRequest(original.ID, turn, owner.actor, owner.purpose, original.Model)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordSystemPrompt(original.ID, turn, owner.actor, request, "keep unrelated request instructions"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RequestMessages(original.ID, turn, owner.purpose, request, []llm.Message{{Type: llm.TypeAssistant, Content: "keep unrelated audit"}}); err != nil {
			t.Fatal(err)
		}
		appendStatus(owner.actor, map[string]any{"type": "request_retry", "request_id": request, "purpose": "compaction"})
		if err := s.FinishRequest(request, RequestCompletion{Status: "completed"}); err != nil {
			t.Fatal(err)
		}
	}
	appendStatus("main", map[string]any{"type": "runtime_event", "body": map[string]any{"type": "job_exit"}})
	appendStatus("main", map[string]any{"type": "compaction_failed", "purpose": "compaction"})
	appendStatus("main/child_tail", map[string]any{"type": "child_state", "status": "running"})
	if _, err := s.Append(original.ID, turn, "main/child_tail", "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "concurrent child reply"}); err != nil {
		t.Fatal(err)
	}
	// A runtime recovery warning can refer to the compaction request but is real
	// visible context, not a request audit status, and must survive the handoff.
	if _, err := s.Append(original.ID, turn, "main", "message", "developer", true, llm.Message{Type: llm.TypeRuntimeInstruction, RequestID: mainRequest, Content: "keep recovery warning"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(original.ID, turn, "main", "status", "user", true, map[string]any{"type": "visible_status", "request_id": mainRequest}); err != nil {
		t.Fatal(err)
	}
	call, err := s.CallIntent(original.ID, turn, "main", coding, llm.ToolCall{ID: "tail-write", Name: "write", Arguments: json.RawMessage(`{"path":"tail.txt"}`)})
	if err != nil {
		t.Fatal(err)
	}
	fileTip, err := s.CommitChange(original.ID, call, []string{"tail.txt"}, true)
	if err != nil {
		t.Fatal(err)
	}
	resultID, err := s.CallResult(original.ID, turn, "main", call, json.RawMessage(`{"ok":true}`), nil, map[string]any{"exact": "tool execution record"}, render.Markdown{Summary: "write tail", Detail: "exact tool details"}, true)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Branch(original.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := s.TranscriptJSONL(original.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.Continue(original.ID, llm.NewCompactionContext("compact summary"), sourceTip, sourceTip+1, []int64{original.EntryTip}, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if continued.FileTip != fileTip {
		t.Fatalf("file tail lost: got %d, want %d", continued.FileTip, fileTip)
	}
	frozen, err := s.Session(original.ID)
	if err != nil || !frozen.ReadOnly {
		t.Fatalf("predecessor not frozen: %+v, %v", frozen, err)
	}
	if after, err := s.Branch(original.ID, 0); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("predecessor entries changed: %v", err)
	}
	if after, err := s.TranscriptJSONL(original.ID, 0); err != nil || !bytes.Equal(exact, after) {
		t.Fatalf("predecessor exact export changed: %v", err)
	}
	for i, id := range auditIDs {
		entry, err := s.Entry(id)
		if err != nil {
			t.Fatal(err)
		}
		var body struct{ Message llm.Message }
		if err := json.Unmarshal(entry.Content, &body); err != nil || !reflect.DeepEqual(body.Message, input[i]) {
			t.Fatalf("original request message/replay audit changed at %d: %+v, %v", i, body.Message, err)
		}
	}
	entries, err := s.Branch(continued.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	copied := make(map[int64]Entry)
	for _, entry := range entries[1:] { // The first entry is the new summary.
		copied[entry.EventSeq()] = entry
	}
	wantCopies := 1 // Separately retained original human input.
	for _, old := range before {
		if old.ID <= sourceTip {
			continue
		}
		var status struct {
			Type      string
			RequestID int64 `json:"request_id"`
		}
		if err := json.Unmarshal(old.Content, &status); err != nil {
			t.Fatal(err)
		}
		got, found := copied[old.EventSeq()]
		if old.Kind == "status" && !old.Visible && status.Type == "request_message" && status.RequestID == mainRequest {
			if found {
				t.Fatalf("main compaction audit copied: %s", old.Content)
			}
			continue
		}
		wantCopies++
		if !found || got.Source != old.EventSeq() || got.Actor != old.Actor || got.Kind != old.Kind || got.Role != old.Role || got.Visible != old.Visible || got.TurnID != old.TurnID || got.FileTip != old.FileTip || got.MainTurnID != old.MainTurnID || got.UndoOwnerTurnID != old.UndoOwnerTurnID || !bytes.Equal(got.Content, old.Content) {
			t.Fatalf("real tail changed or disappeared: old=%+v new=%+v", old, got)
		}
	}
	if len(copied) != wantCopies || copied[original.EntryTip].ID == 0 {
		t.Fatalf("continuation copies=%d, want %d including retained human input", len(copied), wantCopies)
	}
	if _, err := s.ReadInspection(t.Context(), copied[resultID].ID); err != nil {
		t.Fatalf("copied tool record missing: %v", err)
	}
	continuedExact, err := s.TranscriptJSONL(continued.ID, 0)
	if err != nil || bytes.Contains(continuedExact, []byte("original-native-prefix")) || bytes.Contains(continuedExact, []byte("summary-native-reply")) || bytes.Contains(continuedExact, []byte("summary audit reply")) {
		t.Fatalf("continuation retained summarized replay/request audit: %v", err)
	}
}

func TestContinueDropsInheritedMainCompactionMessageAudit(t *testing.T) {
	s, original, turn, _ := historyFixture(t)
	request, err := s.StartRequest(original.ID, turn, "main", "compaction", original.Model)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := s.RequestMessages(original.ID, turn, "compaction", request, []llm.Message{{Type: llm.TypeAssistant, Content: "old copied request audit"}})
	if err != nil {
		t.Fatal(err)
	}
	audit, err := s.Entry(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	cut, err := s.Session(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.Continue(original.ID, llm.NewCompactionContext("first summary"), cut.EntryTip, cut.EntryTip+1, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate a continuation written before audit filtering, including its
	// original source identity and request still owned by the frozen root.
	if err := s.transact(func(tx *sql.Tx) error {
		_, err := appendTx(tx, current.ID, audit.TurnID, audit.Actor, audit.Kind, audit.Role, audit.Visible, audit.Content, audit.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(current.ID, turn, "main", "status", "", false, map[string]any{"type": "runtime_event", "body": map[string]string{"type": "timer"}}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Branch(current.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.Continue(current.ID, llm.NewCompactionContext("second summary"), before[0].ID, before[1].ID, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s.Branch(continued.ID, 0)
	if err != nil || len(entries) != 2 || entries[1].EventSeq() != before[2].EventSeq() || !bytes.Equal(entries[1].Content, before[2].Content) {
		t.Fatalf("inherited audit kept or unrelated runtime tail lost: %+v, %v", entries, err)
	}
	if after, err := s.Branch(current.ID, 0); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("old continuation audit changed: %v", err)
	}
	var owner string
	if err := s.db.QueryRow("SELECT session_id FROM model_requests WHERE id=?", request).Scan(&owner); err != nil || owner != original.ID {
		t.Fatalf("original request owner changed: %q, %v", owner, err)
	}
}

func TestMessagesAcquireOnlyVisibleBodiesAcrossHiddenRequestAudit(t *testing.T) {
	s, original, turn, _ := historyFixture(t)
	loaded, err := s.Load(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, err := s.Messages(loaded.ID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := s.StartRequest(loaded.ID, turn, "main/child_audit", "compaction", loaded.Model)
	if err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(map[string]string{"opaque": strings.Repeat("hidden-native-audit-", 1<<16)})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := s.RequestMessages(loaded.ID, turn, "compaction", request, []llm.Message{{
		Type: llm.TypeAssistant, Content: "large hidden child summary input",
		State: &llm.ReplayState{Provider: "script", Model: loaded.Model.Model.ID, Version: 1, Items: []json.RawMessage{state}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.Append(loaded.ID, turn, "main", "status", "", false, map[string]any{"type": "runtime_event", "body": map[string]string{"type": "job_exit"}})
	if err != nil {
		t.Fatal(err)
	}
	notice := llm.Message{Type: llm.TypeRuntimeNotification, Content: `{"type":"job_exit"}`, EventSeq: event, RequestID: request}
	if _, err := s.Append(loaded.ID, turn, "main", "message", notice.Role(), true, notice); err != nil {
		t.Fatal(err)
	}
	want = append(want, notice)
	for _, tip := range []int64{0, ids[0]} {
		all, err := s.Branch(loaded.ID, tip)
		if err != nil {
			t.Fatal(err)
		}
		var wantVisible []Entry
		foundAudit := false
		for _, entry := range all {
			if entry.Visible {
				wantVisible = append(wantVisible, entry)
			}
			foundAudit = foundAudit || (entry.ID == ids[0] && bytes.Contains(entry.Content, state))
		}
		if !foundAudit {
			t.Fatal("full branch lost exact hidden audit body")
		}
		visible, err := branchWith(s.db, loaded.ID, tip, true)
		if err != nil || !reflect.DeepEqual(visible, wantVisible) {
			t.Fatalf("visible acquisition changed entry metadata or stopped at hidden ancestor: %v", err)
		}
		if visible[0].Source != original.EntryTip || visible[0].EventSeq() != original.EntryTip {
			t.Fatalf("visible acquisition lost imported source identity: %+v", visible[0])
		}
	}
	got, err := s.Messages(loaded.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("visible message projection changed: got=%+v want=%+v err=%v", got, want, err)
	}
}
