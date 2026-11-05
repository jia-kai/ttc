package history

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestForkCoreSharesBalancedPrefixAndKeepsIndependentBaseline(t *testing.T) {
	s, source, turn, request := historyFixture(t)
	call, err := s.CallIntent(source.ID, turn, "main", request, llm.ToolCall{ID: "file", Name: "write", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitChange(source.ID, call, []string{"local.txt"}, true); err != nil {
		t.Fatal(err)
	}
	runtimeID, err := s.Append(source.ID, turn, "main", "message", "developer", true, llm.Message{Type: llm.TypeRuntimeContext, Content: "source runtime only"})
	if err != nil {
		t.Fatal(err)
	}
	balanced, err := s.Append(source.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "complete prefix"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Assistant(source.ID, turn, "main", request, llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "pending", Name: "read", Arguments: json.RawMessage(`{}`)}}}); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := s.db.QueryRow("SELECT count(*) FROM entries").Scan(&before); err != nil {
		t.Fatal(err)
	}
	fork, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	var baseSession string
	var base, after, owned int
	if err := s.db.QueryRow("SELECT base_session_id,base_entry_id FROM sessions WHERE id=?", fork.ID).Scan(&baseSession, &base); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM entries").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM entries WHERE session_id=?", fork.ID).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if baseSession != source.ID || int64(base) != balanced || before != after || owned != 0 || fork.EntryTip != balanced || fork.UndoFloor != balanced || fork.FileTip != 0 || fork.RedoTip != 0 {
		t.Fatal("invalid shared-prefix baseline", baseSession, base, before, after, owned, fork)
	}
	entries, err := s.Branch(fork.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.SessionID != source.ID || entry.FileTip != 0 || entry.Source != 0 || entry.ID == runtimeID && entry.Visible {
			t.Fatal("shared entry lost provenance/projection", entry)
		}
	}
	original, err := s.Entry(runtimeID)
	if err != nil || !original.Visible || original.FileTip == 0 {
		t.Fatal("fork changed original evidence", original, err)
	}
	local, err := s.Append(fork.ID, "", "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "fork local"})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Entry(local)
	if err != nil || entry.Parent != balanced || entry.FileTip != 0 || entry.SessionID != fork.ID {
		t.Fatal("fork adopted source file/runtime ownership", entry, err)
	}
	nested, err := s.Load(fork.ID)
	if err != nil || nested.EntryTip != local || nested.FileTip != 0 {
		t.Fatal("fork-of-fork lost selected local suffix", nested, err)
	}
	if err := validateHistoryTip(s.db, nested.ID, balanced); err != nil {
		t.Fatal("nested inherited cut rejected", err)
	}
	late, err := s.Append(source.ID, "", "main", "status", "", false, map[string]string{"type": "late_source"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateHistoryTip(s.db, fork.ID, late); err == nil {
		t.Fatal("later same-lineage source row was accepted")
	}
	if _, err := s.Branch(fork.ID, late); err == nil {
		t.Fatal("Branch accepted unrelated source tip")
	}
	if err := validateHistoryTip(s.db, fork.ID, 0); err != nil {
		t.Fatal("explicit empty cut rejected", err)
	}
	if _, err := forkBaseWith(s.db, "missing"); err == nil {
		t.Fatal("missing fork source accepted")
	}
	if _, err := s.ReadContextState(context.Background(), nested.ID); err != nil {
		t.Fatal("nested context cannot read shared current tip", err)
	}
}

func TestForkScanDoesNotTransferHiddenTextReplayOrAuditBodies(t *testing.T) {
	s, source, turn, _ := historyFixture(t)
	large := strings.Repeat("hidden payload ", 1<<19)
	message := llm.Message{Type: llm.TypeAssistant, Content: large, State: &llm.ReplayState{Provider: "fixture", Model: "fixture", Items: []json.RawMessage{json.RawMessage(`{"payload":"` + large + `"}`)}}}
	if _, err := s.Append(source.ID, turn, "main/child_hidden", "message", "assistant", false, message); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(source.ID, turn, "main", "status", "", false, map[string]any{"type": "request_message", "message": map[string]string{"content": large}}); err != nil {
		t.Fatal(err)
	}
	entries, err := forkScanWith(s.db, source.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	bytes := 0
	for _, entry := range entries {
		bytes += len(entry.Content)
	}
	if bytes > 4096 {
		t.Fatal("lightweight fork scan transferred hidden payloads", bytes)
	}
	if _, err := s.Load(source.ID); err != nil {
		t.Fatal(err)
	}
}

func TestForkScanPreservesCanonicalEnvelopeValidation(t *testing.T) {
	for _, body := range []string{
		`{"Type":"assistant","CONTENT":123}`,
		`{"type":"assistant","content":"ok","state":{"provider":"p","items":123}}`,
		`{"type":"user_input","calls":[{"id":"call"}]}`,
		`{"type":"assistant","files":[{"path":"x","bytes":"bad"}]}`,
		`{"type":"assistant","runtime":false}`,
	} {
		t.Run(body, func(t *testing.T) {
			s, source, turn, _ := historyFixture(t)
			id, err := s.Append(source.ID, turn, "main/child", "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "valid"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE entries SET content_json=? WHERE id=?", body, id); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(source.ID); err == nil {
				t.Fatal("invalid hidden canonical envelope accepted")
			}
		})
	}
}

func TestForkCoreEmptyPrefixAndUnselectedSourceSibling(t *testing.T) {
	s, source, _, _ := historyFixture(t)
	empty := NewID("session")
	if _, err := s.db.Exec(`INSERT INTO sessions(id,workspace_id,lineage_id,name,name_source,model_json,last_activity_ms,metadata_json)
	 SELECT ?,workspace_id,lineage_id,'Empty','manual',model_json,last_activity_ms,'{}' FROM sessions WHERE id=?`, empty, source.ID); err != nil {
		t.Fatal(err)
	}
	fork, err := s.Load(empty)
	if err != nil || fork.EntryTip != 0 || fork.FileTip != 0 || fork.UndoFloor != 0 {
		t.Fatal("empty prefix fork failed", fork, err)
	}
	var baseSession string
	if err := s.db.QueryRow("SELECT base_session_id FROM sessions WHERE id=?", fork.ID).Scan(&baseSession); err != nil || baseSession != empty {
		t.Fatal("empty fork lost explicit source context", baseSession, err)
	}
	sibling, err := s.Append(source.ID, "", "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "abandoned sibling"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE sessions SET active_entry_id=NULL WHERE id=?", source.ID); err != nil {
		t.Fatal(err)
	}
	selected, err := s.Append(source.ID, "", "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "selected independent root"})
	if err != nil {
		t.Fatal(err)
	}
	fork, err = s.Load(source.ID)
	if err != nil || fork.EntryTip != selected {
		t.Fatal(fork, err)
	}
	if err := validateHistoryTip(s.db, fork.ID, sibling); err == nil {
		t.Fatal("older same-owner source sibling entered frozen ancestry")
	}
	if err := validateHistoryTip(s.db, source.ID, sibling); err != nil {
		t.Fatal("local abandoned branch must remain inspectable", err)
	}
}
