package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scicode/internal/history"
	"scicode/internal/provider"
	"scicode/internal/render"
)

func branchWrite(t *testing.T, w *Manager, saved history.Session, turn, actor string, request int64, body string) int64 {
	t.Helper()
	_, calls, err := w.Store.Assistant(saved.ID, turn, actor, request, provider.Message{Role: "assistant", Calls: []provider.ToolCall{{ID: history.NewID("call"), Name: "write", Arguments: []byte(`{"path":"result"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Apply(context.Background(), saved.ID, calls[0], []Mutation{{Path: "result", Data: []byte(body)}}); err != nil {
		t.Fatal(err)
	}
	id, err := w.Store.CallResult(saved.ID, turn, actor, calls[0], []byte(`{"ok":true}`), nil, render.Markdown{Revision: 1, Summary: "write result"}, actor == "main")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestBranchRestoreMixedActorsSiblingFilesAndShellConflict(t *testing.T) {
	w, saved, turn, request := fixture(t)
	branchWrite(t, w, saved, turn, "main", request, "main content")
	childReq, err := w.Store.StartRequest(saved.ID, turn, "child", "coding", saved.Model)
	if err != nil {
		t.Fatal(err)
	}
	a := branchWrite(t, w, saved, turn, "child", childReq, "child content")
	if err = w.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	restore := func(id int64) error {
		target, err := w.Store.BranchSelectionTarget(saved.ID, id)
		if err != nil {
			return err
		}
		return w.Restore(context.Background(), saved.ID, "branch", target)
	}
	if err = restore(saved.EntryTip); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(w.Root, "result")); !os.IsNotExist(err) {
		t.Fatal("new file survived baseline restore", err)
	}
	turn, err = w.Store.BeginTurn(saved.ID, "user", saved.Model)
	if err != nil {
		t.Fatal(err)
	}
	request, err = w.Store.StartRequest(saved.ID, turn, "main", "coding", saved.Model)
	if err != nil {
		t.Fatal(err)
	}
	b := branchWrite(t, w, saved, turn, "main", request, "sibling content")
	if err = os.WriteFile(filepath.Join(w.Root, "shell-side-effect"), []byte("outside undo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = restore(a); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(w.Root, "result")); err != nil || string(body) != "child content" {
		t.Fatal(string(body), err)
	}
	if body, err := os.ReadFile(filepath.Join(w.Root, "shell-side-effect")); err != nil || string(body) != "outside undo" {
		t.Fatal("shell effects were restored", string(body), err)
	}
	if err = os.WriteFile(filepath.Join(w.Root, "result"), []byte("external change"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = restore(b); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatal("shell interference was not rejected", err)
	}
	current, err := w.Store.Session(saved.ID)
	if err != nil || current.EntryTip != a {
		t.Fatal("failed restore changed selected branch", current, err)
	}
	if body, err := os.ReadFile(filepath.Join(w.Root, "result")); err != nil || string(body) != "external change" {
		t.Fatal("failed restore changed files", string(body), err)
	}
}
