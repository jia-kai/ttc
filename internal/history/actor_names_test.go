package history

import (
	"context"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestSubagentNamesSurviveClosedChildAndCompaction(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	actor, name := "main/child_opaque", "four word research helper"
	for _, state := range []string{"running", "idle", "closed"} {
		if _, err := s.RecordChildState(session.ID, ChildSnapshot{ChildID: actor, TurnID: turn, Label: name, ContextState: state, Outcome: "completed"}); err != nil {
			t.Fatal(err)
		}
	}
	retained, err := s.Append(session.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "Retain this"})
	if err != nil {
		t.Fatal(err)
	}
	cut, err := s.Session(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.Continue(session.ID, llm.NewCompactionContext("summary"), cut.EntryTip, retained, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{session.ID, continued.ID} {
		names, err := s.SubagentNames(context.Background(), id)
		if err != nil || len(names) != 1 || names[actor] != name {
			t.Fatal(names, err)
		}
	}
	if _, err := s.RecordChildState(continued.ID, ChildSnapshot{ChildID: actor, Label: "different", ContextState: "closed", Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubagentNames(context.Background(), continued.ID); err == nil {
		t.Fatal("accepted conflicting durable names")
	}
}
