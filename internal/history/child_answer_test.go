package history

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
	"ttc/internal/render"
)

func TestReadChildAnswerUsesImmutableAssignmentMetadata(t *testing.T) {
	for _, mode := range []string{"notification", "omitted", "tool result"} {
		t.Run(mode, func(t *testing.T) {
			s, session, mainTurn, request := historyFixture(t)
			child := "main/child_metadata"
			model := session.Model
			model.Model.ID = "saved-child-model"
			turn, err := s.BeginChildTurn(session.ID, child, model)
			if err != nil {
				t.Fatal(err)
			}
			response, err := s.Append(session.ID, turn, child, "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "# Saved answer\n\n- first\n- second\n\n```go\nanswer()\n```"})
			if err != nil {
				t.Fatal(err)
			}
			easy := true
			finish := ChildFinish{ChildID: child, Label: "Original label", Easy: &easy, TurnID: turn, JobID: "job_old", Outcome: "completed", ContextState: "idle", Persistent: true, ResultEntry: response, Answer: "preview"}
			seq, err := s.FinishChildTurn(session.ID, finish)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE turns SET started_ms=1000,finished_ms=4345 WHERE id=?", turn); err != nil {
				t.Fatal(err)
			}
			finish.Type = "child_turn_finished"
			raw, err := json.Marshal(finish)
			if err != nil {
				t.Fatal(err)
			}
			var id int64
			if mode == "tool result" {
				raw, err = json.Marshal(map[string]any{"child_id": child, "child_turn_id": turn, "job_id": "job_old", "result_entry_id": response, "assignment_status": "completed", "persistent": true, "kind": "subagent", "finish_event_seq": seq})
				if err != nil {
					t.Fatal(err)
				}
				call := llm.ToolCall{ID: "child-metadata", Name: "subagent", Arguments: json.RawMessage(`{"persistent":true,"label":"Original label"}`)}
				if _, err := s.Append(session.ID, mainTurn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{call}}); err != nil {
					t.Fatal(err)
				}
				callID, err := s.CallIntent(session.ID, mainTurn, "main", request, call)
				if err != nil {
					t.Fatal(err)
				}
				id, err = s.CallResult(session.ID, mainTurn, "main", callID, raw, nil, map[string]any{"result": finish}, render.Tool("subagent", call.Arguments, raw), true)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if mode == "omitted" {
					raw = json.RawMessage(`{"type":"child_turn_finished","child_id":"` + child + `","output_omitted":true}`)
				}
				id, err = s.Append(session.ID, mainTurn, "main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: string(raw), EventSeq: seq})
				if err != nil {
					t.Fatal(err)
				}
			}
			// Reusing the child must not redirect old inspectors to a new turn,
			// model, label or job, even though it is the same retained actor.
			newModel := model
			newModel.Model.ID = "current-reused-model"
			later, err := s.BeginChildTurn(session.ID, child, newModel)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(session.ID, later, child, "status", "", false, map[string]any{"type": "job_completion", "job": map[string]any{"job_id": "job_later", "label": "Later label", "easy": false}}); err != nil {
				t.Fatal(err)
			}
			check := func(id int64) {
				t.Helper()
				answer, err := s.ReadChildAnswer(context.Background(), id)
				if err != nil || answer.ResponseID != response || answer.ChildID != child || answer.Label != "Original label" || answer.TurnID != turn || answer.JobID != "job_old" || answer.Outcome != "completed" || answer.Model != "saved-child-model" {
					t.Fatalf("wrong immutable metadata: %+v, %v", answer, err)
				}
				if answer.Easy == nil || !*answer.Easy || answer.Persistent == nil || !*answer.Persistent || answer.ElapsedMS == nil || *answer.ElapsedMS != 3345 || answer.StartedMS == nil || *answer.StartedMS != 1000 || answer.FinishedMS == nil || *answer.FinishedMS != 4345 {
					t.Fatalf("missing saved assignment fields: %+v", answer)
				}
			}
			check(id)
			loaded, err := s.Load(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateHistoryTip(s.db, loaded.ID, id); err != nil {
				t.Fatal(err)
			}
			check(id)
		})
	}
}

func TestReadChildAnswerMissingHistoricalMetadataIsUnavailable(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	response, err := s.Append(session.ID, "", "main/child_old", "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "saved"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"type": "child_turn_finished", "child_id": "main/child_old", "result_entry_id": response})
	id, err := s.Append(session.ID, turn, "main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := s.ReadChildAnswer(context.Background(), id)
	if err != nil || answer.ResponseID != response || answer.Easy != nil || answer.Persistent != nil || answer.ElapsedMS != nil || answer.StartedMS != nil || answer.FinishedMS != nil || answer.Model != "" || answer.TurnID != "" || answer.JobID != "" {
		t.Fatalf("invented old metadata: %+v, %v", answer, err)
	}
	// Authored JSON cannot grant runtime provenance or access a saved child.
	spoof, err := s.Append(session.ID, turn, "main", "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	answer, err = s.ReadChildAnswer(context.Background(), spoof)
	if err != nil || answer.ResponseID != 0 {
		t.Fatalf("authored JSON gained answer access: %+v, %v", answer, err)
	}
}

func TestReadChildAnswerDoesNotAllocateNativeEnvelopeAndPagesFullMarkdown(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	content := "# Saved heading\n\n- first\n- second\n\n```go\n" + strings.Repeat("savedCode()\n", 3000) + "```\n\nSAVED ANSWER TAIL"
	message := llm.Message{Type: llm.TypeAssistant, Content: content}
	if err := message.AppendState(session.Model, 1, json.RawMessage(`{"native":"`+strings.Repeat("x", 4<<20)+`"}`)); err != nil {
		t.Fatal(err)
	}
	response, err := s.Append(session.ID, "", "main/child_large", "message", "assistant", false, message)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"type": "child_turn_finished", "child_id": "main/child_large", "result_entry_id": response})
	id, err := s.Append(session.ID, turn, "main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	result := testing.Benchmark(func(b *testing.B) {
		for range b.N {
			answer, err := s.ReadChildAnswer(context.Background(), id)
			if err != nil || answer.ResponseID != response {
				b.Fatal(answer, err)
			}
			page, err := s.InspectPage(context.Background(), answer.ResponseID, 0, InspectionPageChars)
			if err != nil || len(page.Text) > 4*InspectionPageChars || !page.LargeEnvelope {
				b.Fatal(page, err)
			}
		}
	})
	if bytes := result.AllocedBytesPerOp(); bytes > 512<<10 {
		t.Fatalf("metadata and one bounded page allocated %d bytes/op for a 4 MiB native envelope", bytes)
	}
	var saved strings.Builder
	for offset := 0; ; offset += InspectionPageChars {
		page, err := s.InspectPage(context.Background(), response, offset, InspectionPageChars)
		if err != nil {
			t.Fatal(err)
		}
		saved.WriteString(page.Text)
		if offset+page.Limit >= page.Total {
			break
		}
	}
	if saved.String() != content {
		t.Fatal("bounded reads did not preserve full saved Markdown")
	}
}

func TestReadChildAnswerDoesNotSearchHistoricalJobOrLabelRecords(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	child := "main/child_historical"
	response, err := s.Append(session.ID, "", child, "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "saved answer"})
	if err != nil {
		t.Fatal(err)
	}
	appendRecord := func(actor, kind, role string, visible bool, body any) int64 {
		t.Helper()
		id, err := s.Append(session.ID, turn, actor, kind, role, visible, body)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	appendRecord(child, "status", "", false, map[string]any{"type": "child_state", "child": ChildSnapshot{ChildID: child, Label: "Historical label", TurnID: "missing-turn", JobID: "saved-job"}})
	// Snapshot's omitempty encoding represents the saved false easy flag.
	appendRecord(child, "status", "", false, map[string]any{"type": "job_completion", "job": map[string]any{"job_id": "saved-job", "kind": "subagent", "started_at": "2026-01-01T00:00:00Z", "finished_at": "2026-01-01T00:00:04.5Z"}})
	finish := ChildFinish{Type: "child_turn_finished", ChildID: child, TurnID: "missing-turn", JobID: "saved-job", ResultEntry: response, Outcome: "failed", ContextState: "closed"}
	raw, _ := json.Marshal(finish)
	id := appendRecord("main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: string(raw)})
	answer, err := s.ReadChildAnswer(context.Background(), id)
	if err != nil || answer.Label != "" || answer.Easy != nil || answer.ElapsedMS != nil || answer.Model != "" {
		t.Fatalf("guessed historical metadata from unrelated records: %+v, %v", answer, err)
	}
	// The omitted-source boundary must still reject a spoofed source identity.
	spoof := appendRecord("main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, EventSeq: id, Content: `{"type":"child_turn_finished","child_id":"` + child + `","output_omitted":true}`})
	if _, err := s.ReadChildAnswer(context.Background(), spoof); err == nil {
		t.Fatal("accepted non-status immutable source")
	}
}

func TestReadChildAnswerCanonicalMetadataAndImmediateBackgroundCompletion(t *testing.T) {
	for _, easy := range []bool{false, true} {
		for _, form := range []string{"direct", "source", "omitted source", "duplicate source"} {
			t.Run(fmt.Sprintf("easy=%t/%s", easy, form), func(t *testing.T) {
				s, session, _, _ := historyFixture(t)
				child := "main/child_canonical"
				turn, err := s.BeginChildTurn(session.ID, child, session.Model)
				if err != nil {
					t.Fatal(err)
				}
				response, err := s.Append(session.ID, turn, child, "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "saved answer"})
				if err != nil {
					t.Fatal(err)
				}
				// Case-folded fields and last duplicate/non-null scalar values
				// must agree with the canonical Go completion decoder.
				body := fmt.Sprintf(`{"TYPE":"child_turn_finished","CHILD_ID":%q,"result_entry_id":%d,"child_turn_id":"wrong-first-turn","CHILD_TURN_ID":%q,"child_turn_id":null,"JOB_ID":"saved-job","OUTCOME":"completed","PERSISTENT":true,"persistent":null,"LABEL":"wrong-first-label","label":"Saved label","label":null,"EASY":%t,"ANSWER":"saved answer"}`, child, response, turn, easy)
				var seq int64
				if form != "direct" {
					source := any(json.RawMessage(body))
					if form == "duplicate source" {
						source = json.RawMessage(`{"type":"not-a-wrapper","TYPE":"runtime_event","BODY":` + body + `}`)
					}
					actor := child
					if form == "duplicate source" {
						actor = "main"
					}
					seq, err = s.Append(session.ID, "", actor, "status", "", false, source)
					if err != nil {
						t.Fatal(err)
					}
					// Model-facing API projection deliberately does not expose
					// saved label/easy metadata. No job_completion exists yet.
					body = fmt.Sprintf(`{"type":"child_turn_finished","child_id":%q,"result_entry_id":%d,"child_turn_id":%q,"job_id":"saved-job"}`, child, response, turn)
					if form == "omitted source" {
						body = fmt.Sprintf(`{"type":"child_turn_finished","child_id":%q,"output_omitted":true}`, child)
					}
				}
				id, err := s.Append(session.ID, "", "main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: body, EventSeq: seq})
				if err != nil {
					t.Fatal(err)
				}
				answer, err := s.ReadChildAnswer(context.Background(), id)
				if err != nil || answer.ResponseID != response || answer.TurnID != turn || answer.JobID != "saved-job" || answer.Label != "Saved label" || answer.Easy == nil || *answer.Easy != easy || answer.Persistent == nil || !*answer.Persistent || answer.Model != session.Model.Model.ID {
					t.Fatalf("canonical immediate metadata missing/misattributed: %+v, %v", answer, err)
				}
			})
		}
	}
}

func TestReadChildAnswerPointLookupPlansAndLineageScale(t *testing.T) {
	s, session, _, _ := historyFixture(t)
	child := "main/child_scale"
	turn, err := s.BeginChildTurn(session.ID, child, session.Model)
	if err != nil {
		t.Fatal(err)
	}
	response, err := s.Append(session.ID, turn, child, "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "saved answer"})
	if err != nil {
		t.Fatal(err)
	}
	easy := false
	finish := ChildFinish{ChildID: child, Label: "Saved label", Easy: &easy, TurnID: turn, JobID: "saved-job", Outcome: "completed", ContextState: "closed", ResultEntry: response}
	seq, err := s.FinishChildTurn(session.ID, finish)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"type":"child_turn_finished","child_id":%q,"result_entry_id":%d}`, child, response)
	id, err := s.Append(session.ID, "", "main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: body, EventSeq: seq})
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		name, sql, point string
		args             []any
	}{{"completion source", childCompletionSourceQuery, "SEARCH entries USING INTEGER PRIMARY KEY", []any{seq}}, {"assignment turn", childAnswerTurnQuery, "SEARCH t USING INDEX sqlite_autoindex_turns_1", []any{session.ID, turn, child}}} {
		rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query.sql, query.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		text := strings.Join(plan, "\n")
		t.Logf("%s plan:\n%s", query.name, text)
		if !strings.Contains(text, query.point) || strings.Contains(text, "entry_source") || strings.Contains(text, "SCAN entries") || strings.Contains(text, "SCAN t") {
			t.Fatalf("metadata must use primary-key identity lookups, not history scans: %s", text)
		}
	}
	measure := func() time.Duration {
		start := time.Now()
		for range 100 {
			answer, err := s.ReadChildAnswer(context.Background(), id)
			if err != nil || answer.ResponseID != response || answer.Label != "Saved label" || answer.Easy == nil || *answer.Easy {
				t.Fatalf("wrong bounded metadata: %+v %v", answer, err)
			}
		}
		return time.Since(start) / 100
	}
	before := measure()
	if _, err := s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000)
	 INSERT INTO entries(session_id,actor_id,kind,model_visible,content_json,created_ms)
	 SELECT ?,?,'status',0,'{"type":"unrelated"}',0 FROM n`, session.ID, child); err != nil {
		t.Fatal(err)
	}
	t.Logf("metadata read mean: small lineage %s; after 100000 same-lineage entries %s (EXPLAIN above enforces bounded access)", before, measure())
}
