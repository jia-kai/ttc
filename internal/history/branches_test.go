package history

import (
	"context"
	"strings"
	"testing"

	"ttc/internal/provider"
	"ttc/internal/render"
)

func TestHistoryTreeShowsSiblingBranchesAndBoundedLabels(t *testing.T) {
	s, saved, turn, _ := historyFixture(t)
	baseline := saved.EntryTip
	a, err := s.Append(saved.ID, turn, "main", "message", "assistant", true, provider.Message{Role: "assistant", Content: "Old branch evidence"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CommitRestore(saved.ID, RestoreTarget{EntryTip: baseline}); err != nil {
		t.Fatal(err)
	}
	b, err := s.Append(saved.ID, turn, "main", "message", "assistant", true, provider.Message{Role: "assistant", Content: "\x1b[31mNew branch\n" + strings.Repeat("x", 1<<20)})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.HistoryTree(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Current != b || len(tree.Nodes) < 4 {
		t.Fatal(tree)
	}
	for _, node := range tree.Nodes {
		if node.ID == a && node.Selected || node.ID == b && (node.Parent != baseline || !node.Selected) {
			t.Fatal(node)
		}
		if strings.ContainsAny(node.Label, "\x1b\n") || len(node.Label) > 768 {
			t.Fatal("unbounded or unsafe label", node)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.HistoryTree(ctx, saved.ID); err == nil {
		t.Fatal("ignored canceled metadata query")
	}
}

func TestBranchSelectionRequiresCompleteMainToolBatch(t *testing.T) {
	s, saved, turn, request := historyFixture(t)
	assistant, calls, err := s.Assistant(saved.ID, turn, "main", request, provider.Message{Role: "assistant", Calls: []provider.ToolCall{
		{ID: "first", Name: "read", Arguments: []byte(`{"path":"one"}`)},
		{ID: "second", Name: "read", Arguments: []byte(`{"path":"two"}`)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.CallResult(saved.ID, turn, "main", calls[0], []byte(`{"ok":true}`), nil, render.Markdown{Revision: 1, Summary: "read one"}, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CallResult(saved.ID, turn, "main", calls[1], []byte(`{"ok":true}`), nil, render.Markdown{Revision: 1, Summary: "read two"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, tip := range []int64{assistant, first} {
		if _, err = s.BranchSelectionTarget(saved.ID, tip); err == nil || !strings.Contains(err.Error(), "pending main tool") {
			t.Fatal(tip, err)
		}
	}
	if target, err := s.BranchSelectionTarget(saved.ID, second); err != nil || target.EntryTip != second {
		t.Fatal(target, err)
	}
	tree, err := s.HistoryTree(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range tree.Nodes {
		if node.ID >= assistant && node.ID < second && node.Restorable || node.ID == second && !node.Restorable {
			t.Fatal("unsafe selectable tool boundary", node)
		}
	}
}

func TestBranchSelectionHonorsFloorAncestryAndReadOnly(t *testing.T) {
	s, saved, turn, _ := historyFixture(t)
	baseline := saved.EntryTip
	a, err := s.Append(saved.ID, turn, "main", "status", "", false, map[string]string{"text": "Boundary on first branch"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CommitRestore(saved.ID, RestoreTarget{EntryTip: baseline}); err != nil {
		t.Fatal(err)
	}
	b, err := s.Append(saved.ID, turn, "main", "status", "", false, map[string]string{"text": "Later sibling"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("UPDATE sessions SET undo_floor_id=? WHERE id=?", a, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BranchSelectionTarget(saved.ID, b); err == nil || !strings.Contains(err.Error(), "undo boundary") {
		t.Fatal("later sibling crossed the floor", err)
	}
	if _, err = s.BranchSelectionTarget(saved.ID, a); err != nil {
		t.Fatal(err)
	}
	tree, err := s.HistoryTree(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range tree.Nodes {
		if node.ID == b && node.Restorable || node.ID == a && !node.Restorable {
			t.Fatal(node)
		}
	}
	if _, err = s.DB.Exec("UPDATE sessions SET read_only=1 WHERE id=?", saved.ID); err != nil {
		t.Fatal(err)
	}
	tree, err = s.HistoryTree(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range tree.Nodes {
		if node.Restorable || node.Reason != "read-only session" {
			t.Fatal(node)
		}
	}
	if _, err = s.BranchSelectionTarget(saved.ID, a); err == nil {
		t.Fatal("restored read-only history")
	}
	if _, err = s.BranchSelectionTarget(saved.ID, -1); err == nil {
		t.Fatal("accepted negative entry ID")
	}
}

func TestHistoryTreeIdentifiesHumanInputsAndAuthoredLabels(t *testing.T) {
	s, saved, turn, _ := historyFixture(t)
	runtimeID, err := s.Append(saved.ID, turn, "main", "message", "user", true, provider.Message{Role: "user", Content: "Job finished", Runtime: true})
	if err != nil {
		t.Fatal(err)
	}
	childID, err := s.Append(saved.ID, turn, "child", "message", "user", false, provider.Message{Role: "user", Content: "Child question"})
	if err != nil {
		t.Fatal(err)
	}
	authored := "Explain attached notes."
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	_, inputID, err := s.AdmitTurn(saved.ID, "user", saved.Model, &provider.Message{Role: "user", Content: authored + "\nATTACHMENT CONTENT", UserText: &authored})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.HistoryTree(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, node := range tree.Nodes {
		if node.UserInput {
			count++
		}
		if (node.ID == runtimeID || node.ID == childID) && node.UserInput {
			t.Fatal("runtime/child message marked as user checkpoint", node)
		}
		if node.ID == inputID && (!node.UserInput || node.Label != authored) {
			t.Fatal("authored input not identified", node)
		}
	}
	if count != 2 {
		t.Fatal("wrong human checkpoint count", count)
	}
}
