package history

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestRuntimeNotificationSourceBoundsLargePayload(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	text := strings.Repeat("timer body ", 800000)
	id, err := s.Append(v.ID, "", "main", "status", "", false, map[string]any{"type": "runtime_event", "body": map[string]any{"type": "wakeup", "wakeup_id": "timer", "fired_count": 7, "message": text}})
	if err != nil {
		t.Fatal(err)
	}
	bounded, omitted, err := s.RuntimeNotificationSource(id, 6144)
	if err != nil || !omitted || bounded.EventSeq != id || len(bounded.Body) > 1024 || strings.Contains(string(bounded.Body), "timer body") {
		t.Fatal("large body escaped bounded source reader", len(bounded.Body), omitted, err)
	}
	var ref struct {
		ID    string `json:"wakeup_id"`
		Fired int    `json:"fired_count"`
	}
	if err := json.Unmarshal(bounded.Body, &ref); err != nil || ref.ID != "timer" || ref.Fired != 7 {
		t.Fatal(ref, err)
	}
	full, omitted, err := s.RuntimeNotificationSource(id, 0)
	if err != nil || omitted || !strings.Contains(string(full.Body), text) {
		t.Fatal("durable timer message changed", err)
	}
}

func TestOmittedChildNoticeViewRestoresOnlyPresentation(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "recovered"}[recovered], func(t *testing.T) {
			s, v, turn, _ := historyFixture(t)
			answer := strings.Repeat("Answer evidence. ", 400)
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
			bounded, omitted, err := s.RuntimeNotificationSource(seq, 6144)
			var source ChildFinish
			if err != nil || !omitted || json.Unmarshal(bounded.Body, &source) != nil || !source.Persistent || source.Answer != "" || source.ChildID != finish.ChildID {
				t.Fatal("bounded canonical child metadata changed", string(bounded.Body), err)
			}
			body, _ := json.Marshal(map[string]any{"type": "child_turn_finished", "child_id": finish.ChildID, "output_omitted": true})
			message := llm.Message{Type: llm.TypeRuntimeNotification, EventSeq: seq, Content: string(body)}
			id, err := s.Append(v.ID, turn, "main", "message", "user", true, message)
			if err != nil {
				t.Fatal(err)
			}
			view, err := s.ReadEntryView(context.Background(), id)
			if err != nil || view.Message == nil || !strings.Contains(view.Message.Content, answer) {
				t.Fatal("missing source-backed preview", view, err)
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
				if _, err := omittedChildNoticeSource(s.db, invalid); err == nil {
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
