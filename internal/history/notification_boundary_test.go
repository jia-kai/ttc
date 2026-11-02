package history

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestOmittedNotificationBoundaryExportsOnlyExplicitSource(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "recovered"}[recovered], func(t *testing.T) {
			s, session, turn, _ := historyFixture(t)
			child := "main/child_boundary"
			const private = "PRIVATE CHILD TOOL AND REASONING CONTENT"
			if _, err := s.Append(session.ID, "", child, "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: private}); err != nil {
				t.Fatal(err)
			}
			answer := strings.Repeat("Full explicit answer. ", 1000) + "FULL ANSWER TAIL"
			result, err := s.Append(session.ID, "", child, "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: answer})
			if err != nil {
				t.Fatal(err)
			}
			finish := ChildFinish{Type: "child_turn_finished", ChildID: child, TurnID: "assignment", JobID: "job", Outcome: "completed", ContextState: "closed", ResultEntry: result, Answer: answer[:MaxChildAnswerBytes], Truncated: true}
			seq, err := s.Append(session.ID, "", child, "status", "", false, finish)
			if err != nil {
				t.Fatal(err)
			}
			if recovered {
				seq, err = s.Append(session.ID, "", "main", "status", "", false, map[string]any{"type": "runtime_event", "body": finish, "recovered_from_event_seq": seq})
				if err != nil {
					t.Fatal(err)
				}
			}
			// ResultEntry is deliberately absent from delivery: export must close
			// the explicit source's reference, not guess from child history.
			body := `{"type":"child_turn_finished","child_id":"` + child + `","output_omitted":true}`
			message := llm.Message{Type: llm.TypeRuntimeNotification, Content: body, EventSeq: seq}
			delivery, err := s.Append(session.ID, turn, "main", "message", "user", true, message)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(session.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "done"}); err != nil {
				t.Fatal(err)
			}
			for generation := range 3 {
				id := delivery
				if generation > 0 {
					session, err = s.Load(session.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := s.db.QueryRow("SELECT id FROM entries WHERE session_id=? AND source_id=?", session.ID, delivery).Scan(&id); err != nil {
						t.Fatal(err)
					}
				}
				view, err := s.ReadEntryView(context.Background(), id)
				if err != nil || view.Message == nil || !reflect.DeepEqual(*view.Message, message) || view.ChildCompletion == nil || view.ChildCompletion.Answer != finish.Answer {
					t.Fatalf("generation %d: distinct delivery and bounded display: %+v, %v", generation, view, err)
				}
				got, err := s.ChildResponse(id)
				if err != nil || got != result {
					t.Fatalf("explicit result reference: %d, %v", got, err)
				}
				markdown, err := s.ActorTranscript(session.ID, "main", id)
				if err != nil || !strings.Contains(string(markdown), answer) || strings.Contains(string(markdown), private) {
					t.Fatalf("full explicit answer export or privacy: %v", err)
				}
				exact, err := s.ActorTranscriptJSONL(session.ID, "main", id)
				if err != nil || !strings.Contains(string(exact), "output_omitted") || !strings.Contains(string(exact), "child_turn_finished") || strings.Contains(string(exact), "FULL ANSWER TAIL") || strings.Contains(string(exact), private) {
					t.Fatalf("exact archive must contain delivery+explicit completion, not child conversation: %v", err)
				}
			}
		})
	}
}

func TestOmittedNotificationSharedReferenceValidation(t *testing.T) {
	for _, test := range []string{"missing source", "future source", "copied source", "wrong kind", "wrong source actor", "wrong delivery actor", "wrong child", "wrong assignment", "wrong job", "wrong result", "foreign lineage", "foreign workspace", "oversized answer"} {
		t.Run(test, func(t *testing.T) {
			s, session, turn, _ := historyFixture(t)
			finish := ChildFinish{Type: "child_turn_finished", ChildID: "main/child_boundary", TurnID: "assignment", JobID: "job", Answer: "display answer"}
			if test == "oversized answer" {
				finish.Answer = strings.Repeat("x", MaxChildAnswerBytes+1)
			}
			seq, err := s.Append(session.ID, "", finish.ChildID, "status", "", false, finish)
			if err != nil {
				t.Fatal(err)
			}
			notice := map[string]any{"type": finish.Type, "child_id": finish.ChildID, "child_turn_id": finish.TurnID, "job_id": finish.JobID, "output_omitted": true}
			sourceSeq, actor := seq, "main"
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := s.db.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			switch test {
			case "missing source":
				sourceSeq = 0
			case "future source":
				sourceSeq = seq + 100
			case "copied source":
				exec("UPDATE entries SET source_id=? WHERE id=?", seq-1, seq)
			case "wrong kind":
				exec("UPDATE entries SET kind='summary' WHERE id=?", seq)
			case "wrong source actor":
				exec("UPDATE entries SET actor_id='main/child_other' WHERE id=?", seq)
			case "wrong delivery actor":
				actor = "main/child_other"
			case "wrong child":
				notice["child_id"] = "main/child_other"
			case "wrong assignment":
				notice["child_turn_id"] = "other"
			case "wrong job":
				notice["job_id"] = "other"
			case "wrong result":
				notice["result_entry_id"] = seq - 1
			case "foreign lineage", "foreign workspace":
				foreign, _ := startHistorySession(t, s, t.TempDir(), session.Model, "foreign")
				if test == "foreign lineage" {
					exec("UPDATE sessions SET workspace_id=? WHERE id=?", session.WorkspaceID, foreign.ID)
				} else {
					exec("UPDATE sessions SET lineage_id=? WHERE id=?", session.LineageID, foreign.ID)
				}
				exec("UPDATE entries SET session_id=? WHERE id=?", foreign.ID, seq)
			}
			body, _ := json.Marshal(notice)
			id, err := s.Append(session.ID, turn, actor, "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: string(body), EventSeq: sourceSeq})
			if err != nil {
				t.Fatal(err)
			}
			entry, err := s.Entry(id)
			if err != nil {
				t.Fatal(err)
			}
			for name, read := range map[string]func() error{
				"display":    func() error { _, err := s.ReadEntryView(context.Background(), id); return err },
				"inspection": func() error { _, err := s.ChildResponse(id); return err },
				"markdown":   func() error { _, err := s.ExportText(entry); return err },
				"jsonl":      func() error { _, err := s.ActorTranscriptJSONL(session.ID, actor, id); return err },
			} {
				if err := read(); err == nil {
					t.Errorf("%s accepted invalid notification source", name)
				}
			}
		})
	}
}
