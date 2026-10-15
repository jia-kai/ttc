package history

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/render"
)

func TestRecordChildStateStorageContract(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	snapshot := ChildSnapshot{ChildID: "main/child", Label: "research helper", ContextState: "idle", Outcome: "completed", TurnID: turn, JobID: "job"}
	id, err := s.RecordChildState(session.ID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Entry(id)
	if err != nil || entry.Actor != snapshot.ChildID || entry.TurnID != turn || entry.Kind != "status" || entry.Visible {
		t.Fatal(entry, err)
	}
	var event struct {
		Type  string        `json:"type"`
		Child ChildSnapshot `json:"child"`
	}
	if err := json.Unmarshal(entry.Content, &event); err != nil || event.Type != "child_state" || event.Child != snapshot {
		t.Fatal(event, err)
	}
	var raw struct {
		Child map[string]json.RawMessage `json:"child"`
	}
	if err := json.Unmarshal(entry.Content, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Child) != 6 || string(raw.Child["outcome"]) != `"completed"` || string(raw.Child["context_state"]) != `"idle"` {
		t.Fatal("incorrect snapshot storage fields", string(entry.Content))
	}
}

func TestFinishChildTurnStateInvariants(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled", "running", ""} {
		for _, persistent := range []bool{false, true} {
			for _, state := range []string{"idle", "closed", "running", "", "unknown"} {
				t.Run(fmt.Sprintf("%s/persistent=%t/%s", status, persistent, state), func(t *testing.T) {
					s, session, _, _ := historyFixture(t)
					turn, err := s.BeginChildTurn(session.ID, "main/child", llm.Selection{})
					if err != nil {
						t.Fatal(err)
					}
					finish := ChildFinish{ChildID: "main/child", TurnID: turn, JobID: "job", Outcome: status, Persistent: persistent, ContextState: state}
					id, err := s.FinishChildTurn(session.ID, finish)
					validStatus := status == "completed" || status == "failed" || status == "cancelled"
					validState := state == "closed" || state == "idle" && persistent && status == "completed"
					if !validStatus || !validState {
						if err == nil {
							t.Fatal("accepted invalid completion", finish)
						}
						var saved string
						if err := s.DB.QueryRow("SELECT status FROM turns WHERE id=?", turn).Scan(&saved); err != nil || saved != "running" {
							t.Fatal("invalid write changed turn", saved, err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					entry, err := s.Entry(id)
					if err != nil {
						t.Fatal(err)
					}
					decoded, err := readChildFinish(entry.Content)
					if err != nil || decoded.Outcome != status || decoded.ContextState != state {
						t.Fatal("incorrect saved contract", string(entry.Content), err)
					}
					if _, err := s.FinishChildTurn(session.ID, finish); err == nil {
						t.Fatal("accepted duplicate terminal write")
					}
				})
			}
		}
	}
}

func TestReadChildFinishCurrentFormat(t *testing.T) {
	prefix := `{"type":"child_turn_finished","child_id":"main/child","child_turn_id":"turn","job_id":"job",`
	for _, tc := range []struct {
		name, fields string
		valid        bool
	}{
		{"current", `"outcome":"completed","context_state":"closed"`, true},
		{"persistent idle", `"outcome":"completed","persistent":true,"context_state":"idle"`, true},
		{"unknown state", `"outcome":"completed","context_state":"unknown"`, true},
		{"failed unknown state", `"outcome":"failed","context_state":"unknown"`, true},
		{"cancelled unknown state", `"outcome":"cancelled","context_state":"unknown"`, true},
		{"missing state", `"outcome":"completed"`, false},
		{"provenance cannot replace state", `"outcome":"completed","recovered_from_event_seq":42`, false},
		{"provenance is opaque", `"outcome":"completed","context_state":"unknown","recovered_from_event_seq":{"original":42}`, true},
		{"no status alias", `"status":"completed","context_state":"closed"`, false},
		{"no assignment status alias", `"assignment_status":"completed","context_state":"closed"`, false},
		{"no child state alias", `"outcome":"completed","child_state":"closed"`, false},
		{"invalid outcome type", `"outcome":1,"context_state":"closed"`, false},
		{"null outcome", `"outcome":null,"context_state":"closed"`, false},
		{"null state", `"outcome":"completed","context_state":null`, false},
		{"empty state", `"outcome":"completed","context_state":""`, false},
		{"running state", `"outcome":"completed","context_state":"running"`, false},
		{"running outcome", `"outcome":"running","context_state":"closed"`, false},
		{"nonpersistent idle", `"outcome":"completed","context_state":"idle"`, false},
		{"failed idle", `"outcome":"failed","persistent":true,"context_state":"idle"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := json.RawMessage(prefix + tc.fields + `,"extra":{"exact":9007199254740993}}`)
			_, err := readChildFinish(body)
			if (err == nil) != tc.valid {
				t.Fatalf("decode valid=%t: %v", tc.valid, err)
			}
			object, err := recoveryBody(body)
			if (err == nil) != tc.valid {
				t.Fatalf("recovery valid=%t: %v", tc.valid, err)
			}
			if !tc.valid {
				return
			}
			if string(object["extra"]) != `{"exact":9007199254740993}` {
				t.Fatal("lost unknown fields or precision", object)
			}
		})
	}
}

func TestReadChildFinishRejectsInvalidTypeAndObjects(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `1`, `"child_turn_finished"`, `{}`, `{"type":"job_exit","outcome":"completed","context_state":"closed"}`, `{"type":null}`} {
		if _, err := readChildFinish(json.RawMessage(body)); err == nil {
			t.Fatal("accepted invalid child completion", body)
		}
	}
}

func TestChildRecoveryPreservesCanonicalBodyAndOriginalEvidence(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested=%t", nested), func(t *testing.T) {
			s, source, turn, request := historyFixture(t)
			canonical := `{"type":"child_turn_finished","child_id":"main/child","child_turn_id":"stored-turn","job_id":"job","outcome":"completed","context_state":"unknown","recovered_from_event_seq":9007199254740993,"extra":{"exact":9007199254740993}}`
			evidence := `{"type":"child_turn_finished","child_id":"main/child","child_turn_id":"stored-turn","job_id":"job","status":"completed","extra":{"exact":9007199254740993}}`
			var eventID int64
			var err error
			if nested {
				eventID = appendRecoveryRuntime(t, s, source.ID, canonical)
			} else {
				// Direct canonical events have no recovery envelope provenance.
				canonical = strings.Replace(canonical, `,"recovered_from_event_seq":9007199254740993`, `,"provenance":{"source":9007199254740993}`, 1)
				eventID, err = s.Append(source.ID, "", "main/child", "status", "", false, json.RawMessage(canonical))
				if err != nil {
					t.Fatal(err)
				}
				entry, _ := s.Entry(eventID)
				if label := s.Label(entry); !strings.Contains(label, "stored-turn") || !strings.Contains(label, "done") {
					t.Fatal("inspection label", label)
				}
			}
			messageID, err := s.Append(source.ID, turn, "main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: evidence})
			if err != nil {
				t.Fatal(err)
			}
			_, calls, err := s.Assistant(source.ID, turn, "main", request, llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "child", Name: "subagent", Arguments: []byte(`{}`)}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CallResult(source.ID, turn, "main", calls[0], []byte(evidence), nil, map[string]any{"original": json.RawMessage(evidence)}, render.Markdown{}, true); err != nil {
				t.Fatal(err)
			}
			if _, err := s.StartRequest(source.ID, turn, "main", "compaction", source.Model); err != nil {
				t.Fatal(err)
			}
			archive, err := s.ArchiveActorTranscript(source.ID, "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			archiveBefore, err := os.ReadFile(archive + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			before := sourceRecoveryState(t, s, source.ID)
			current := source
			for range 2 {
				loaded, err := s.Load(current.ID)
				if err != nil {
					t.Fatal(err)
				}
				notices := recoveryNotices(t, s, loaded.ID)
				if len(notices) != 1 {
					t.Fatal(notices)
				}
				if string(notices[0].Body) != canonical {
					t.Fatal("changed canonical recovery body", string(notices[0].Body))
				}
				var copiedMessage string
				if err := s.DB.QueryRow("SELECT content_json FROM entries WHERE session_id=? AND source_id=?", loaded.ID, messageID).Scan(&copiedMessage); err != nil || !strings.Contains(copiedMessage, `\"status\":\"completed\"`) || strings.Contains(copiedMessage, "assignment_status") {
					t.Fatal("rewrote copied message evidence", copiedMessage, err)
				}
				current = loaded
			}
			if after := sourceRecoveryState(t, s, source.ID); before != after {
				t.Fatal("rewrote original entries, turns or model requests")
			}
			var result string
			if err := s.DB.QueryRow("SELECT result_json FROM tool_calls WHERE id=?", calls[0]).Scan(&result); err != nil || result != evidence {
				t.Fatal("rewrote original tool result", result, err)
			}
			archiveAfter, err := os.ReadFile(archive + ".jsonl")
			if err != nil || string(archiveBefore) != string(archiveAfter) {
				t.Fatal("rewrote original archive", err)
			}
		})
	}
}
