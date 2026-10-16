package history

import (
	"encoding/json"
	"strings"
	"testing"
	"ttc/internal/llm"
	"ttc/internal/render"
)

func TestChildResponseRequiresRuntimeProvenanceAndMatchingSource(t *testing.T) {
	s, session, _, _ := historyFixture(t)
	source, err := s.Append(session.ID, "", "main/child_test", "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "full response"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := startHistorySession(t, s, t.TempDir(), session.Model, "foreign")
	// Isolate workspace rejection from lineage rejection.
	if _, err := s.db.Exec("UPDATE sessions SET lineage_id=? WHERE id=?", session.LineageID, foreign.ID); err != nil {
		t.Fatal(err)
	}
	foreignSource, err := s.Append(foreign.ID, "", "main/child_test", "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "foreign response"})
	if err != nil {
		t.Fatal(err)
	}
	var root string
	if err := s.db.QueryRow("SELECT path FROM workspaces WHERE id=?", session.WorkspaceID).Scan(&root); err != nil {
		t.Fatal(err)
	}
	sameWorkspace, _ := startHistorySession(t, s, root, session.Model, "different lineage")
	sameWorkspaceSource, err := s.Append(sameWorkspace.ID, "", "main/child_test", "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "same workspace, foreign lineage"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		runtime bool
		child   string
		target  int64
		want    int64
		wantErr bool
	}{
		{"runtime", true, "main/child_test", source, source, false},
		{"task prompt", false, "main/child_test", source, 0, false},
		{"wrong child", true, "main/child_other", source, 0, true},
		{"foreign workspace same lineage", true, "main/child_test", foreignSource, 0, true},
		{"foreign lineage same workspace", true, "main/child_test", sameWorkspaceSource, 0, true},
		{"no committed response", true, "main/child_test", 0, 0, false},
		{"missing reference", true, "main/child_test", source + 10000, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(ChildFinish{Type: "child_turn_finished", ChildID: tc.child, ResultEntry: tc.target, Outcome: "failed", ContextState: "closed", Warning: "preserved warning"})
			if err != nil {
				t.Fatal(err)
			}
			typ := llm.TypeUserInput
			if tc.runtime {
				typ = llm.TypeRuntimeNotification
			}
			id, err := s.Append(session.ID, "", "main", "message", "user", true, llm.Message{Type: typ, Content: string(raw)})
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.ChildResponse(id)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatal(got, err)
			}
			entry, err := s.Entry(id)
			if err != nil {
				t.Fatal(err)
			}
			var message llm.Message
			if err := json.Unmarshal(entry.Content, &message); err != nil || message.Content != string(raw) {
				t.Fatal("resolution changed completion metadata", err)
			}
		})
	}
}

func TestChildResponseLoadAndLoadOfLoadResolveExactOriginalSource(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreground", true: "background"}[background], func(t *testing.T) {
			s, original, turn, request := historyFixture(t)
			full := strings.Repeat("full response\n", 1000) + "FULL TAIL"
			source, err := s.Append(original.ID, turn, "main/child_test", "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: full})
			if err != nil {
				t.Fatal(err)
			}
			finish := ChildFinish{Type: "child_turn_finished", ChildID: "main/child_test", Outcome: "completed", ContextState: "closed", ResultEntry: source, Answer: full[:MaxChildAnswerBytes], Truncated: true}
			raw, err := json.Marshal(finish)
			if err != nil {
				t.Fatal(err)
			}
			var envelope int64
			if background {
				envelope, err = s.Append(original.ID, turn, "main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: string(raw)})
			} else {
				call := llm.ToolCall{ID: "child", Name: "subagent", Arguments: []byte(`{"label":"helper","persistent":false}`)}
				if _, err := s.Append(original.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{call}}); err != nil {
					t.Fatal(err)
				}
				callID, callErr := s.CallIntent(original.ID, turn, "main", request, call)
				if callErr != nil {
					t.Fatal(callErr)
				}
				envelope, err = s.CallResult(original.ID, turn, "main", callID, raw, nil, map[string]any{"result": finish}, render.Tool("subagent", call.Arguments, raw), true)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(original.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "main finished"}); err != nil {
				t.Fatal(err)
			}
			current := original
			for generation := range 3 {
				entryID := envelope
				if generation > 0 {
					loaded, err := s.Load(current.ID)
					if err != nil {
						t.Fatal(err)
					}
					if loaded.ID == current.ID || loaded.LineageID != original.LineageID || loaded.WorkspaceID != original.WorkspaceID {
						t.Fatal("not an independent same-lineage load", loaded)
					}
					current = loaded
					if err := s.db.QueryRow("SELECT id FROM entries WHERE session_id=? AND source_id=?", current.ID, envelope).Scan(&entryID); err != nil {
						t.Fatal(err)
					}
				}
				got, err := s.ChildResponse(entryID)
				if err != nil || got != source {
					t.Fatal("resolved wrong committed source", generation, got, source, err)
				}
				entry, err := s.Entry(got)
				if err != nil {
					t.Fatal(err)
				}
				var message llm.Message
				if err := json.Unmarshal(entry.Content, &message); err != nil || message.Content != full || entry.SessionID != original.ID {
					t.Fatal("lost original full response", generation, err)
				}
			}
		})
	}
}
