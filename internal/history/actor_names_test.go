package history

import (
	"context"
	"testing"
	"time"

	"ttc/internal/provider"
)

func TestSubagentNamesSurviveClosedChildAndCompaction(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	actor, name := "main/child_opaque", "four word research helper"
	for _, state := range []string{"running", "idle", "closed"} {
		if _, err := s.Append(session.ID, turn, actor, "status", "", false, map[string]any{"type": "child_state", "child": map[string]string{"label": name, "state": state}}); err != nil {
			t.Fatal(err)
		}
	}
	retained, err := s.Append(session.ID, turn, "main", "message", "assistant", true, provider.Message{Role: "assistant", Content: "Retain this"})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := s.ArchiveTranscript(session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.Continue(session.ID, "summary", archive, retained, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{session.ID, continued.ID} {
		names, err := s.SubagentNames(context.Background(), id)
		if err != nil || len(names) != 1 || names[actor] != name {
			t.Fatal(names, err)
		}
	}
	if _, err := s.Append(continued.ID, "", actor, "status", "", false, map[string]any{"type": "child_state", "child": map[string]string{"label": "different"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubagentNames(context.Background(), continued.ID); err == nil {
		t.Fatal("accepted conflicting durable names")
	}
}
