package history

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"ttc/internal/llm"
)

func TestBoundedNotificationPreservesNULAndUTF8Prefixes(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	for _, text := range []string{
		"head\x00" + strings.Repeat("x", 7000),
		"\x00" + strings.Repeat("界", 3000),
		strings.Repeat("界", 3000),
	} {
		id, err := s.Append(v.ID, "", "main", "status", "", false, map[string]any{"type": "job_exit", "job_id": "job", "stdout": text})
		if err != nil {
			t.Fatal(err)
		}
		const limit = 1024
		delivery, err := s.ReadNotificationDelivery(id, limit)
		if err != nil {
			t.Fatal(err)
		}
		var body struct{ Stdout string }
		if err := json.Unmarshal(delivery.Body, &body); err != nil {
			t.Fatal(err)
		}
		expected := text[:limit]
		for !utf8.ValidString(expected) {
			expected = expected[:len(expected)-1]
		}
		if body.Stdout != expected || len(delivery.TruncatedFields) != 1 || delivery.TruncatedFields[0] != "stdout" {
			t.Fatalf("prefix changed: got %d bytes, want %d; clipped=%v", len(body.Stdout), len(expected), delivery.TruncatedFields)
		}
	}
}

func TestHistoricalChildAnswerRecoveryKeepsPriorReadLimit(t *testing.T) {
	finish := ChildFinish{Type: "child_turn_finished", ChildID: "main/child_old", TurnID: "turn", JobID: "job", Outcome: "completed", ContextState: "closed", Answer: strings.Repeat("x", 7<<10)}
	if err := validateChildFinish(finish); err == nil {
		t.Fatal("new completion accepted an answer over 6 KiB")
	}
	body, err := json.Marshal(finish)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recoveryBody(body); err != nil {
		t.Fatal("historical 7-KiB answer cannot recover", err)
	}
	s, v, _, _ := historyFixture(t)
	id, err := s.Append(v.ID, "", finish.ChildID, "status", "", false, finish)
	if err != nil {
		t.Fatal(err)
	}
	if _, metadata, _, err := readChildCompletionSource(s.db, id); err != nil || metadata.Answer != finish.Answer {
		t.Fatal("historical source cannot be inspected", err)
	}
	finish.Answer += strings.Repeat("x", 1025)
	body, _ = json.Marshal(finish)
	if _, err := recoveryBody(body); err == nil {
		t.Fatal("historical answer over 8 KiB accepted")
	}
}

func TestRuntimeNotificationSourceBoundsLargePayload(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	text := strings.Repeat("timer body ", 800000)
	id, err := s.Append(v.ID, "", "main", "status", "", false, map[string]any{"type": "runtime_event", "body": map[string]any{"type": "wakeup", "wakeup_id": "timer", "fired_count": 7, "message": text}})
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := s.ReadNotificationDelivery(id, 6144)
	if err != nil || len(bounded.TruncatedFields) == 0 || bounded.Source.EventSeq != id || len(bounded.Body) > 6500 || !strings.Contains(string(bounded.Body), "timer body") {
		t.Fatal("large body escaped bounded source reader", len(bounded.Body), bounded.TruncatedFields, err)
	}
	var ref struct {
		ID    string `json:"wakeup_id"`
		Fired int    `json:"fired_count"`
	}
	if err := json.Unmarshal(bounded.Body, &ref); err != nil || ref.ID != "timer" || ref.Fired != 7 {
		t.Fatal(ref, err)
	}
	full, err := s.ReadNotificationDelivery(id, 0)
	if err != nil || len(full.TruncatedFields) != 0 || !strings.Contains(string(full.Body), text) {
		t.Fatal("durable timer message changed", err)
	}
}

func TestOmittedChildNoticeViewRestoresOnlyPresentation(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "recovered"}[recovered], func(t *testing.T) {
			s, v, turn, _ := historyFixture(t)
			answer := strings.Repeat("Answer evidence. ", 350)
			finish := ChildFinish{Type: "child_turn_finished", ChildID: "main/child_fixture", JobID: "job_fixture", Outcome: "completed", ContextState: "idle", Persistent: true, Answer: answer}
			childTurn, err := s.BeginChildTurn(v.ID, finish.ChildID, v.Model)
			if err != nil {
				t.Fatal(err)
			}
			finish.TurnID = childTurn
			seq, err := s.FinishChildTurn(v.ID, finish)
			if err != nil {
				t.Fatal(err)
			}
			if recovered {
				seq, err = s.Append(v.ID, "", "main", "status", "", false, map[string]any{"type": "runtime_event", "body": finish, "recovered_from_event_seq": seq})
				if err != nil {
					t.Fatal(err)
				}
			}
			bounded, err := s.ReadNotificationDelivery(seq, 1024)
			var source ChildFinish
			if err != nil || len(bounded.TruncatedFields) == 0 || json.Unmarshal(bounded.Body, &source) != nil || !source.Persistent || len(source.Answer) != 1024 || !strings.HasPrefix(answer, source.Answer) || source.ChildID != finish.ChildID {
				t.Fatal("bounded canonical child metadata changed", string(bounded.Body), err)
			}
			body, _ := json.Marshal(map[string]any{"type": "child_turn_finished", "child_id": finish.ChildID, "output_omitted": true})
			message := llm.Message{Type: llm.TypeRuntimeNotification, EventSeq: seq, Content: string(body)}
			id, err := s.Append(v.ID, turn, "main", "message", "user", true, message)
			if err != nil {
				t.Fatal(err)
			}
			view, err := s.ReadEntryView(context.Background(), id)
			if err != nil || view.Message == nil || view.Message.Content != message.Content || view.ChildCompletion == nil || view.ChildCompletion.Answer != answer || view.ChildCompletion.ChildID != finish.ChildID {
				t.Fatal("canonical delivery or separate source-backed preview changed", view, err)
			}
			replay, err := s.ReadReplay(context.Background(), v.ID, id)
			if err != nil || len(replay.Rows) == 0 {
				t.Fatal("child completion replay failed", err)
			}
			row := replay.Rows[len(replay.Rows)-1]
			if row.ID != id || row.Label != "" || row.Message == nil || row.Message.Content != message.Content || row.ChildCompletion == nil || row.ChildCompletion.Answer != answer || row.ChildCompletion.ChildID != finish.ChildID {
				t.Fatal("replay changed canonical delivery or separate source-backed preview", row)
			}
			entry, err := s.Entry(id)
			if err != nil {
				t.Fatal(err)
			}
			// Presentation and export must reject a source at or after the
			// delivery's semantic identity, even when its physical copy is newer.
			for _, deliverySource := range []int64{seq, seq - 1} {
				invalid := entry
				invalid.Source = deliverySource
				if _, err := s.ReadView(context.Background(), invalid); err == nil {
					t.Fatal("presentation accepted future notification source")
				}
				if _, err := resolveOmittedChildNotice(s.db, invalid); err == nil {
					t.Fatal("export accepted future notification source")
				}
			}
			var saved llm.Message
			if err := json.Unmarshal(entry.Content, &saved); err != nil || saved.Content != message.Content {
				t.Fatal("presentation changed durable model input", saved, err)
			}
			messages, err := s.Messages(v.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range messages {
				if candidate.EventSeq == seq && strings.Contains(candidate.Content, answer) {
					t.Fatal("presentation answer entered model projection")
				}
			}
		})
	}
}
