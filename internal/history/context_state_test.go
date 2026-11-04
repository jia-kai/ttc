package history

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestReadContextState(t *testing.T) {
	s, session, turn := usageFixture(t)
	admitted := admitUsage(t, s, session, turn, true)
	finishUsage(t, s, admitted.RequestID, "completed", &llm.Usage{InputTokens: 123, OutputTokens: 17})
	state, err := s.ReadContextState(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Messages, messages) || !reflect.DeepEqual(state.Usage, readUsage(t, s, session.ID)) {
		t.Fatal("logical context and usage differ", state)
	}
	if _, err := s.Append(session.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "later"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Messages, messages) {
		t.Fatal("later activity changed a returned view")
	}
	state.Messages[0].Content = "caller-owned"
	current, err := s.ReadContextState(context.Background(), session.ID)
	if err != nil || current.Messages[0].Content == "caller-owned" {
		t.Fatal("returned state aliases history", err)
	}
	if _, err := s.ReadContextState(context.Background(), "missing"); err == nil {
		t.Fatal("missing session accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ReadContextState(ctx, session.ID); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not propagated", err)
	}
}

func TestReadSnapshotAllowsCommitWhileViewRemainsFrozen(t *testing.T) {
	s, session, turn := usageFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q, closeSnapshot, err := s.readSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSnapshot()
	var original int64
	if err := q.QueryRow("SELECT active_entry_id FROM sessions WHERE id=?", session.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	// A held snapshot must not occupy the Store's sole commit connection.
	done := make(chan error, 1)
	go func() {
		_, err := s.Append(session.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "concurrent"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		closeSnapshot()
		<-done
		t.Fatal("snapshot blocked commit connection")
	}
	var frozen int64
	if err := q.QueryRow("SELECT active_entry_id FROM sessions WHERE id=?", session.ID).Scan(&frozen); err != nil || frozen != original {
		t.Fatal("read snapshot moved after commit", frozen, original, err)
	}
}
