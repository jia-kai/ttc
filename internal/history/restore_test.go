package history

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestRestorePlanBranchAndRedoSelection(t *testing.T) {
	s, saved, turn, _ := historyFixture(t)
	appendEntry := func(text string) int64 {
		t.Helper()
		id, err := s.Append(saved.ID, turn, "main", "status", "", false, map[string]string{"text": text})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	selectBranch := func(id int64) {
		t.Helper()
		plan, err := s.PlanBranch(saved.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CommitRestore(plan); err != nil {
			t.Fatal(err)
		}
	}
	a := appendEntry("a")
	selectBranch(saved.EntryTip)
	b := appendEntry("b")
	selectBranch(a)
	// Reselecting the active node must not discard the sibling saved for redo.
	selectBranch(a)
	redo, err := s.PlanRedo(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRestore(redo); err != nil {
		t.Fatal(err)
	}
	current, err := s.Session(saved.ID)
	if err != nil || current.EntryTip != b || current.RedoTip != 0 {
		t.Fatal("redo did not restore and consume the saved sibling", current, err)
	}
	if _, err := s.PlanRedo(saved.ID); err == nil || !strings.Contains(err.Error(), "nothing to redo") {
		t.Fatal(err)
	}
	undo, err := s.PlanUndo(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRestore(undo); err != nil {
		t.Fatal(err)
	}
	redo, err = s.PlanRedo(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRestore(redo); err != nil {
		t.Fatal(err)
	}
	current, err = s.Session(saved.ID)
	if err != nil || current.EntryTip != b {
		t.Fatal("undo/redo changed the selected branch", current, err)
	}
}

func TestRestoreChangesOrdersDivergenceAndExcludesSharedPrefix(t *testing.T) {
	s, saved, turn, req := historyFixture(t)
	change := func(label string, reversible bool) int64 {
		t.Helper()
		call, err := s.CallIntent(saved.ID, turn, "main", req, llm.ToolCall{ID: NewID("call"), Name: "write", Arguments: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CommitChange(saved.ID, call, []string{label}, reversible); err != nil {
			t.Fatal(err)
		}
		current, err := s.Session(saved.ID)
		if err != nil {
			t.Fatal(err)
		}
		return current.EntryTip
	}
	selectBranch := func(id int64) {
		t.Helper()
		plan, err := s.PlanBranch(saved.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CommitRestore(plan); err != nil {
			t.Fatal(err)
		}
	}
	// An irreversible shared prefix is not crossed and must not block restore.
	prefix := change("prefix", false)
	change("a1", true)
	a := change("a2", true)
	selectBranch(prefix)
	change("b1", true)
	change("b2", true)
	plan, err := s.PlanBranch(saved.ID, a)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := s.RestoreChanges(plan)
	if err != nil {
		t.Fatal(err)
	}
	want := []RestoreChange{
		{Paths: json.RawMessage(`["b2"]`), Reverse: true},
		{Paths: json.RawMessage(`["b1"]`), Reverse: true},
		{Paths: json.RawMessage(`["a1"]`)},
		{Paths: json.RawMessage(`["a2"]`)},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("restore order = %+v, want %+v", steps, want)
	}
	plan, err = s.PlanBranch(saved.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreChanges(plan); err == nil || !strings.Contains(err.Error(), "non-undoable") {
		t.Fatal("crossed irreversible edit", err)
	}
}

func TestRestorePlanRejectsInvalidForeignStaleAndReadOnly(t *testing.T) {
	s, saved, turn, _ := historyFixture(t)
	other, _, _, _ := historyFixture(t)
	plan, err := s.PlanBranch(saved.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		store *Store
		plan  RestorePlan
	}{{s, RestorePlan{}}, {other, plan}} {
		if _, err := invalid.store.RestoreChanges(invalid.plan); err == nil {
			t.Fatal("accepted unissued plan")
		}
		if err := invalid.store.CommitRestore(invalid.plan); err == nil {
			t.Fatal("committed unissued plan")
		}
	}
	id, err := s.Append(saved.ID, turn, "main", "status", "", false, map[string]string{"text": "new evidence"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreChanges(plan); err == nil {
		t.Fatal("resolved stale plan")
	}
	if err := s.CommitRestore(plan); err == nil {
		t.Fatal("committed stale plan")
	}
	plan, err = s.PlanBranch(saved.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE sessions SET read_only=1 WHERE id=?", saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreChanges(plan); err == nil {
		t.Fatal("resolved read-only plan")
	}
	if err := s.CommitRestore(plan); err == nil {
		t.Fatal("committed read-only plan")
	}
	current, err := s.Session(saved.ID)
	if err != nil || current.EntryTip != id {
		t.Fatal("rejected plan changed history", current, err)
	}
}
