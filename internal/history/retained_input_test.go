package history

import (
	"context"
	"testing"
	"time"
	"ttc/internal/llm"
)

func TestRetainedInputPreInputBranchReplacementAndLoad(t *testing.T) {
	s, original, turn, _ := historyFixture(t)
	tail, err := s.Append(original.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "Completed model work."})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	cut, err := s.Session(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.Continue(original.ID, llm.NewCompactionContext("Summary before retained input."), cut.EntryTip, tail, []int64{original.EntryTip}, time.Now().Add(time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s.Branch(continued.ID, 0)
	if err != nil || len(entries) < 3 {
		t.Fatal(entries, err)
	}
	input := entries[1]
	tree, err := s.HistoryTree(context.Background(), continued.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, node := range tree.Nodes {
		if node.ID == input.ID {
			found = node.UserInput && node.Parent == entries[0].ID
		}
	}
	if !found {
		t.Fatal("retained input has no direct pre-input baseline", tree)
	}
	target, err := s.BranchSelectionTarget(continued.ID, input.Parent)
	if err != nil || target.EntryTip != input.Parent {
		t.Fatal(target, err)
	}
	if err := s.CommitRestore(continued.ID, target); err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(continued.ID)
	if err != nil || len(messages) != 1 {
		t.Fatal(messages, err)
	}
	loaded, err := s.Load(continued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if messages, err := s.Messages(loaded.ID); err != nil || len(messages) != 1 {
		t.Fatal(messages, err)
	}
	replacement := llm.Message{Type: llm.TypeUserInput, Content: "Replacement task."}
	turn, id, err := s.AdmitTurn(continued.ID, "user", continued.Model, &replacement)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Entry(id)
	if err != nil || entry.Parent != input.Parent {
		t.Fatal(entry, err)
	}
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	undo, err := s.UndoTarget(continued.ID)
	if err != nil || undo.EntryTip != input.Parent {
		t.Fatal(undo, err)
	}
	if err := s.CommitRestore(continued.ID, undo); err != nil {
		t.Fatal(err)
	}
	if messages, err := s.Messages(continued.ID); err != nil || len(messages) != 1 {
		t.Fatal(messages, err)
	}
	unchanged, err := s.Entry(input.ID)
	if err != nil || unchanged.Parent != input.Parent || string(unchanged.Content) != string(input.Content) {
		t.Fatal("abandoned input changed", unchanged, err)
	}
}
