package history

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"ttc/internal/llm"
)

func TestAppendAndReadCommittedMessage(t *testing.T) {
	s, saved, turn, _ := historyFixture(t)
	text := "A child assignment"
	message := llm.Message{Type: llm.TypeUserInput, Content: text + " with attachment", UserText: &text, InputSource: "task"}
	committed, err := s.AppendMessage(saved.ID, turn, "main/child_message", message)
	if err != nil || committed.EntryID == 0 || committed.CreatedMS <= 0 || !reflect.DeepEqual(committed.Message, message) {
		t.Fatal(committed, err)
	}
	entry, err := s.Entry(committed.EntryID)
	if err != nil || entry.Visible || entry.Role != "user" || entry.CreatedMS != committed.CreatedMS {
		t.Fatal("message ownership or timestamp differs from receipt", entry, err)
	}
	text = "Caller mutation"
	if committed.Message.DisplayText() != "A child assignment" {
		t.Fatal("receipt aliases caller-owned message data")
	}
	got, err := s.ReadCommittedMessage(context.Background(), committed.EntryID)
	if err != nil || !reflect.DeepEqual(got, committed) {
		t.Fatal("semantic read differs from append receipt", got, committed, err)
	}
	first, err := s.ReadCommittedMessage(context.Background(), saved.EntryTip)
	if err != nil || first.Message.Type != llm.TypeUserInput || first.Message.Content != "hello" || first.CreatedMS <= 0 {
		t.Fatal("initial admitted input metadata was unavailable", first, err)
	}
	messages, err := s.Messages(saved.ID)
	if err != nil || len(messages) != 1 {
		t.Fatal("child message leaked into main input", messages, err)
	}
	status, err := s.Append(saved.ID, "", "main", "status", "", false, map[string]string{"type": "runtime_owned_event"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{0, status} {
		if _, err := s.ReadCommittedMessage(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Fatal("non-message evidence was accepted", id, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ReadCommittedMessage(ctx, committed.EntryID); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored canceled read", err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_append_message BEFORE UPDATE OF active_entry_id ON sessions BEGIN SELECT RAISE(ABORT,'forced message failure'); END`); err != nil {
		t.Fatal(err)
	}
	if failed, err := s.AppendMessage(saved.ID, turn, "main", message); err == nil || !reflect.DeepEqual(failed, CommittedMessage{}) {
		t.Fatal("rolled-back message identity escaped", failed, err)
	}
}

func TestReadNotificationSourceReturnsOnlyOriginalEventIdentity(t *testing.T) {
	s, saved, _, _ := historyFixture(t)
	original, err := s.Append(saved.ID, "", "main", "status", "", false, map[string]any{"type": "runtime_event", "recovered_from_event_seq": 123, "body": map[string]string{"type": "runtime_owned_event"}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.ReadNotificationSource(original)
	if err != nil || source.EventSeq != original || source.RecoveredFrom != 123 {
		t.Fatal(source, err)
	}
	copied, err := s.Append(saved.ID, "", "main", "status", "", false, map[string]string{"type": "runtime_owned_event"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE entries SET source_id=? WHERE id=?", original, copied); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{0, copied, saved.EntryTip} {
		if _, err := s.ReadNotificationSource(id); !errors.Is(err, ErrNotFound) {
			t.Fatal("non-original or non-event evidence was accepted", id, err)
		}
	}
}
