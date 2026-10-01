package workspace

import (
	"context"
	"os"
	"path/filepath"
	"scicode/internal/history"
	"scicode/internal/provider"
	"scicode/internal/scratch"
	"strings"
	"sync"
	"testing"
)

func TestMutationReadsRejectShellGrownFile(t *testing.T) {
	w, saved, _, _ := fixture(t)
	path := filepath.Join(w.Root, "grown")
	if err := os.WriteFile(path, []byte("small"), 0600); err != nil {
		t.Fatal(err)
	}
	state, _, err := w.capture(saved.ID, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := current(path, state); err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatal("conflict check accepted oversized file", err)
	}
	if _, _, err := w.capture(saved.ID, path); err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatal("snapshot accepted oversized file", err)
	}
}

func fixture(t *testing.T) (*Manager, history.Session, string, int64) {
	t.Helper()
	if _, e := scratch.Verify(); e != nil {
		t.Fatal(e)
	}
	s, e := history.Open(filepath.Join(t.TempDir(), "data"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	root := t.TempDir()
	w, e := Open(root, s)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { w.Close() })
	model := provider.Selection{Model: provider.ScriptModel()}
	id := history.NewID("session")
	turn, _, e := s.StartSession(id, root, model, provider.Message{Role: "user", Content: "edit"})
	if e != nil {
		t.Fatal(e)
	}
	v, e := s.Session(id)
	if e != nil {
		t.Fatal(e)
	}
	request, e := s.StartRequest(v.ID, turn, "main", "coding", model)
	if e != nil {
		t.Fatal(e)
	}
	return w, v, turn, request
}
func intent(t *testing.T, w *Manager, v history.Session, turn string, req int64) string {
	t.Helper()
	id, e := w.Store.CallIntent(v.ID, turn, "main", req, provider.ToolCall{ID: history.NewID("p"), Name: "write", Arguments: []byte(`{"path":"x","content":"data"}`)})
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func TestSerializedEditsUndoRedoAndExternalConflict(t *testing.T) {
	w, v, turn, req := fixture(t)
	ctx := context.Background()
	id := intent(t, w, v, turn, req)
	if _, e := w.Apply(ctx, v.ID, id, []Mutation{{Path: "x", Data: []byte("0")}}); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		id := intent(t, w, v, turn, req)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := w.Apply(ctx, v.ID, id, []Mutation{{Path: "x", MustExist: true, Transform: func(b []byte) ([]byte, error) { return append(b, '1'), nil }}}); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	data, _ := os.ReadFile(filepath.Join(w.Root, "x"))
	if len(data) != 9 {
		t.Fatal(string(data))
	}
	w.Store.FinishTurn(turn, "completed")
	w.Store.Append(v.ID, turn, "main", "status", "", false, map[string]string{"text": "done"})
	target, e := w.Store.UndoTarget(v.ID)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(w.Root, "x"), []byte("external"), 0644)
	if e = w.Restore(ctx, v.ID, "undo", target); e == nil {
		t.Fatal("overwrote external change")
	}
	os.WriteFile(filepath.Join(w.Root, "x"), data, 0644)
	if e = w.Restore(ctx, v.ID, "undo", target); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(w.Root, "x")); !os.IsNotExist(e) {
		t.Fatal("undo did not restore absence")
	}
	redo, e := w.Store.BranchTarget(v.ID, target.RedoTip)
	if e != nil {
		t.Fatal(e)
	}
	if e = w.Restore(ctx, v.ID, "redo", redo); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(filepath.Join(w.Root, "x"))
	if string(after) != string(data) {
		t.Fatal(string(after))
	}
}
func TestSymlinksHardlinksAndPreflightAtomicity(t *testing.T) {
	w, v, turn, req := fixture(t)
	outside := filepath.Join(t.TempDir(), "target")
	os.WriteFile(outside, []byte("safe"), 0600)
	os.Symlink(outside, filepath.Join(w.Root, "link"))
	id := intent(t, w, v, turn, req)
	if _, e := w.Apply(context.Background(), v.ID, id, []Mutation{{Path: "good", Data: []byte("x")}, {Path: "link", Data: []byte("bad")}}); e == nil {
		t.Fatal("accepted symlink")
	}
	if _, e := os.Stat(filepath.Join(w.Root, "good")); !os.IsNotExist(e) {
		t.Fatal("preflight changed good path")
	}
	os.Link(outside, filepath.Join(w.Root, "hard"))
	if _, e := w.Apply(context.Background(), v.ID, id, []Mutation{{Path: "hard", Data: []byte("bad")}}); e == nil {
		t.Fatal("accepted hardlink")
	}
}
func TestJournalRecoveryAfterAppliedBytes(t *testing.T) {
	w, v, turn, req := fixture(t)
	id := intent(t, w, v, turn, req)
	blob, e := w.Store.Artifact(v.ID, "snapshots", []byte("new"))
	if e != nil {
		t.Fatal(e)
	}
	state := State{Exists: true, Mode: 0644, Hash: hash([]byte("new")), Blob: blob}
	manifest := Manifest{Version: 1, CallID: id, Reversible: true, Changes: []PathChange{{Path: filepath.Join(w.Root, "x"), After: state}}}
	if e = w.Store.Prepare(v.ID, "apply", manifest); e != nil {
		t.Fatal(e)
	}
	if e = apply(filepath.Join(w.Root, "x"), state); e != nil {
		t.Fatal(e)
	}
	if e = w.Recover(); e != nil {
		t.Fatal(e)
	}
	pending, e := w.Store.PendingOperation()
	if e != nil || pending != nil {
		t.Fatal(pending, e)
	}
	changes, e := w.Store.Session(v.ID)
	if e != nil || changes.FileTip == 0 {
		t.Fatal(changes, e)
	}
}

func TestRestartJournalThenCallRecoveryPreservesRedoTip(t *testing.T) {
	if _, e := scratch.Verify(); e != nil {
		t.Fatal(e)
	}
	root, data := t.TempDir(), filepath.Join(t.TempDir(), "data")
	store, e := history.Open(data)
	if e != nil {
		t.Fatal(e)
	}
	model := provider.Selection{Model: provider.ScriptModel()}
	id := history.NewID("session")
	turn, _, e := store.StartSession(id, root, model, provider.Message{Role: "user", Content: "write"})
	if e != nil {
		t.Fatal(e)
	}
	v, e := store.Session(id)
	if e != nil {
		t.Fatal(e)
	}
	req, e := store.StartRequest(v.ID, turn, "main", "coding", model)
	if e != nil {
		t.Fatal(e)
	}
	_, ids, e := store.Assistant(v.ID, turn, "main", req, provider.Message{Role: "assistant", Calls: []provider.ToolCall{{ID: "p", Name: "write", Arguments: []byte(`{"path":"x","content":"restored"}`)}}})
	if e != nil {
		t.Fatal(e)
	}
	blob, e := store.Artifact(v.ID, "snapshots", []byte("restored"))
	if e != nil {
		t.Fatal(e)
	}
	state := State{Exists: true, Mode: 0644, Hash: hash([]byte("restored")), Blob: blob}
	manifest := Manifest{Version: 1, CallID: ids[0], Reversible: true, Changes: []PathChange{{Path: filepath.Join(root, "x"), After: state}}}
	if e = store.Prepare(v.ID, "apply", manifest); e != nil {
		t.Fatal(e)
	}
	if e = apply(filepath.Join(root, "x"), state); e != nil {
		t.Fatal(e)
	}
	store.Close()
	store, e = history.Open(data)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	w, e := Open(root, store)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	target, e := store.UndoTarget(v.ID)
	if e != nil {
		t.Fatal(e)
	}
	if e = w.Restore(context.Background(), v.ID, "undo", target); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(root, "x")); !os.IsNotExist(e) {
		t.Fatal("undo failed")
	}
	redo, e := store.BranchTarget(v.ID, target.RedoTip)
	if e != nil {
		t.Fatal(e)
	}
	if e = w.Restore(context.Background(), v.ID, "redo", redo); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(root, "x"))
	if e != nil || string(b) != "restored" {
		t.Fatal(string(b), e)
	}
}

func TestOutsideWorkspaceEditIsExplicitlyNonUndoable(t *testing.T) {
	w, v, turn, req := fixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	id := intent(t, w, v, turn, req)
	result, e := w.Apply(context.Background(), v.ID, id, []Mutation{{Path: outside, Data: []byte("external")}})
	if e != nil || result.Reversible {
		t.Fatal(result, e)
	}
	w.Store.Append(v.ID, turn, "main", "status", "", false, map[string]string{"text": "changed"})
	w.Store.FinishTurn(turn, "completed")
	target, e := w.Store.UndoTarget(v.ID)
	if e != nil {
		t.Fatal(e)
	}
	if e = w.Restore(context.Background(), v.ID, "undo", target); e == nil {
		t.Fatal("outside edit was undoable")
	}
	data, e := os.ReadFile(outside)
	if e != nil || string(data) != "external" {
		t.Fatal(string(data), e)
	}
}
