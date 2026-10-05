package history

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"ttc/internal/provider"
)

func TestChildFinishValidationMatchesRecovery(t *testing.T) {
	for _, invalid := range []string{"negative result", "missing job", "bad status", "oversized answer", "failed answer"} {
		t.Run(invalid, func(t *testing.T) {
			s, source, _, _ := historyFixture(t)
			turn, err := s.BeginChildTurn(source.ID, "main/child", provider.Selection{})
			if err != nil {
				t.Fatal(err)
			}
			finish := ChildFinish{Type: "child_turn_finished", ChildID: "main/child", TurnID: turn, JobID: "job", Status: "completed"}
			switch invalid {
			case "negative result":
				finish.ResultEntry = -1
			case "missing job":
				finish.JobID = ""
			case "bad status":
				finish.Status = "running"
			case "oversized answer":
				finish.Answer = strings.Repeat("x", MaxChildAnswerBytes+1)
			case "failed answer":
				finish.Status, finish.Answer = "failed", "unexpected answer"
			}
			if _, err := s.FinishChildTurn(source.ID, finish); err == nil {
				t.Fatal("accepted invalid durable finish")
			}
			body, err := json.Marshal(finish)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := recoveryBody(body); err == nil {
				t.Fatal("accepted invalid recovered finish")
			}
		})
	}
}

func TestPendingRecoveryNotificationsCancellation(t *testing.T) {
	s, source, _, _ := historyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.PendingRecoveryNotifications(ctx, source.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled read: %v", err)
	}
	// The store has one connection. Waiting for it must honor the caller's
	// deadline rather than blocking startup behind another transaction.
	conn, err := s.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.PendingRecoveryNotifications(ctx, source.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connection-blocked read: %v", err)
	}
}

func TestPendingRecoveryNotificationsSelectedTipOnly(t *testing.T) {
	s, source, turn, _ := historyFixture(t)
	appendRecoveryChild(t, s, source.ID)
	if _, err := s.StartRequest(source.ID, turn, "main", "compaction", source.Model); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	notices := recoveryNotices(t, s, loaded.ID)
	if len(notices) != 1 {
		t.Fatal(notices)
	}
	entry, err := s.Entry(notices[0].EventSeq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE sessions SET active_entry_id=? WHERE id=?", entry.Parent, loaded.ID); err != nil {
		t.Fatal(err)
	}
	if got := recoveryNotices(t, s, loaded.ID); len(got) != 0 {
		t.Fatal("recovered abandoned-branch event", got)
	}
}

func TestLoadRecoveryDoesNotReviveCoalescedDeliveredTimer(t *testing.T) {
	s, source, turn, _ := historyFixture(t)
	appendRecoveryRuntime(t, s, source.ID, `{"type":"wakeup","wakeup_id":"timer","message":"old","fired_count":1}`)
	latest := appendRecoveryRuntime(t, s, source.ID, `{"type":"wakeup","wakeup_id":"timer","message":"latest","fired_count":2}`)
	if _, err := s.AdmitRequest(source.ID, turn, "main", source.Model, nil, []provider.Message{{Role: "user", Runtime: true, Content: `{"type":"wakeup","wakeup_id":"timer"}`, EventSeq: latest}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRequest(source.ID, turn, "main", "compaction", source.Model); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveryNotices(t, s, loaded.ID); len(got) != 0 {
		t.Fatal("revived superseded firing", got)
	}
}

func TestLoadRecoveryRetainedCopiesUseOriginalDelivery(t *testing.T) {
	s, source, turn, _ := historyFixture(t)
	delivered := appendRecoveryRuntime(t, s, source.ID, `{"type":"job_exit","job_id":"delivered"}`)
	if _, err := s.AdmitRequest(source.ID, turn, "main", source.Model, nil, []provider.Message{{Role: "user", Runtime: true, Content: `{"type":"job_exit"}`, EventSeq: delivered}}, nil); err != nil {
		t.Fatal(err)
	}
	appendRecoveryChild(t, s, source.ID)
	if _, err := s.StartRequest(source.ID, turn, "main", "compaction", source.Model); err != nil {
		t.Fatal(err)
	}
	archive, err := s.ArchiveTranscript(source.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Continue(source.ID, "retained handoff", archive, delivered, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveryNotices(t, s, loaded.ID); len(got) != 1 {
		t.Fatal("copy duplicated or revived event", got)
	}
}

func TestLoadRecoveryIncludesEventsInExcludedUnbalancedSuffix(t *testing.T) {
	s, source, turn, request := historyFixture(t)
	if _, _, err := s.Assistant(source.ID, turn, "main", request, provider.Message{Role: "assistant", Calls: []provider.ToolCall{{ID: "unfinished", Name: "shell", Arguments: []byte(`{}`)}}}); err != nil {
		t.Fatal(err)
	}
	appendRecoveryChild(t, s, source.ID)
	if _, err := s.StartRequest(source.ID, turn, "main", "compaction", source.Model); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveryNotices(t, s, loaded.ID); len(got) != 1 {
		t.Fatal(got)
	}
	messages, err := s.Messages(loaded.ID)
	if err != nil || len(messages) != 1 || len(messages[0].Calls) != 0 {
		t.Fatal("copied partial exchange", messages, err)
	}
	again, err := s.Load(loaded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveryNotices(t, s, again.ID); len(got) != 1 {
		t.Fatal("boundary marker lost on truncated snapshot", got)
	}
}
