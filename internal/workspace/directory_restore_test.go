package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"ttc/internal/history"
	"ttc/internal/llm"
)

func restoreFileTip(t *testing.T, w *Manager, session string, tip int64) {
	t.Helper()
	if err := w.Restore(context.Background(), session, history.RestoreTarget{FileTip: tip}); err != nil {
		t.Fatal(err)
	}
	assertFileTip(t, w, session, tip)
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("expected absence: %s: %v", path, err)
	}
}

// Keep removed directory inodes alive so immediate inode reuse cannot mask a
// stale-identity bug in redo cleanup.
func holdDirectory(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
}

func TestRestoreRecreatedDirectoriesRemainOwned(t *testing.T) {
	w, saved, turn, request := fixture(t)
	result, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{
		{Path: "shared/nested/z", Data: []byte("z")},
		{Path: "shared/nested/a", Data: []byte("a")},
		{Path: "shared/other/b", Data: []byte("b")},
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(w.Root, "shared")
	for cycle := 0; cycle < 3; cycle++ {
		holdDirectory(t, parent)
		restoreFileTip(t, w, saved.ID, 0)
		assertAbsent(t, parent)
		if len(w.directories.owned) != 0 {
			t.Fatalf("removed directory identities retained: %+v", w.directories.owned)
		}
		restoreFileTip(t, w, saved.ID, result.ChangeID)
		for _, path := range []string{"shared/nested/z", "shared/nested/a", "shared/other/b"} {
			assertContents(t, filepath.Join(w.Root, path), filepath.Base(path))
		}
	}
	restoreFileTip(t, w, saved.ID, 0)
	assertAbsent(t, parent)
}

func TestRestoreSharedParentAfterOriginalCreatorIsAbsent(t *testing.T) {
	w, saved, turn, request := fixture(t)
	apply := func(ops ...Mutation) Result {
		t.Helper()
		result, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), ops)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	apply(Mutation{Path: "shared/owner", Data: []byte("owner")}, Mutation{Path: "shared/keep", Data: []byte("keep")})
	withoutOwner := apply(Mutation{Path: "shared/owner", Delete: true})
	withoutFiles := apply(Mutation{Path: "shared/keep", Delete: true})
	parent := filepath.Join(w.Root, "shared")
	for cycle := 0; cycle < 3; cycle++ {
		restoreFileTip(t, w, saved.ID, withoutOwner.ChangeID)
		assertContents(t, filepath.Join(parent, "keep"), "keep")
		assertAbsent(t, filepath.Join(parent, "owner"))
		holdDirectory(t, parent)
		// This restore only touches keep, whose original change never created
		// shared. Ownership must survive the original creator's absence.
		restoreFileTip(t, w, saved.ID, withoutFiles.ChangeID)
		assertAbsent(t, parent)
	}
}

func TestRestoreSiblingBranchesShareRecreatedParents(t *testing.T) {
	w, saved, turn, request := fixture(t)
	preexisting := filepath.Join(w.Root, "preexisting")
	if err := os.Mkdir(preexisting, 0755); err != nil {
		t.Fatal(err)
	}
	branch := func(name string) int64 {
		t.Helper()
		result, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{
			{Path: "preexisting/shared/" + name, Data: []byte(name)},
			{Path: "preexisting/shared/nested/" + name, Data: []byte(name)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.ChangeID
	}
	a := branch("a")
	holdDirectory(t, filepath.Join(preexisting, "shared"))
	restoreFileTip(t, w, saved.ID, 0)
	b := branch("b")
	for cycle := 0; cycle < 3; cycle++ {
		for _, target := range []struct {
			tip          int64
			name, absent string
		}{{a, "a", "b"}, {b, "b", "a"}} {
			restoreFileTip(t, w, saved.ID, target.tip)
			for _, dir := range []string{"shared", "shared/nested"} {
				assertContents(t, filepath.Join(preexisting, dir, target.name), target.name)
				assertAbsent(t, filepath.Join(preexisting, dir, target.absent))
			}
		}
	}
	restoreFileTip(t, w, saved.ID, 0)
	assertAbsent(t, filepath.Join(preexisting, "shared"))
	if info, err := os.Stat(preexisting); err != nil || !info.IsDir() {
		t.Fatalf("cleanup removed preexisting parent: %v", err)
	}
}

func TestFailedMutationDoesNotCleanEarlierOwnedParent(t *testing.T) {
	w, saved, turn, request := fixture(t)
	var createdTip int64
	for _, op := range []Mutation{{Path: "owned/file", Data: []byte("file")}, {Path: "owned/file", Delete: true}} {
		result, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{op})
		if err != nil {
			t.Fatal(err)
		}
		if !op.Delete {
			createdTip = result.ChangeID
		}
	}
	failure := errors.New("injected write failure")
	w.atomicFile = func(string, []byte, os.FileMode) error { return failure }
	if _, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{{Path: "owned/file", Data: []byte("new")}}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(w.Root, "owned")); err != nil || !info.IsDir() {
		t.Fatalf("failed attempt removed a directory it did not create: %v", err)
	}
	if err := w.Restore(context.Background(), saved.ID, history.RestoreTarget{FileTip: createdTip}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(w.Root, "owned")); err != nil || !info.IsDir() {
		t.Fatalf("failed restore removed a directory it did not create: %v", err)
	}
}

func TestHistoricalDirectoryMetadataDoesNotGrantOwnership(t *testing.T) {
	for _, identities := range []bool{false, true} {
		t.Run(map[bool]string{false: "paths-only", true: "matching-identities"}[identities], func(t *testing.T) {
			w, saved, turn, request := fixture(t)
			parent := filepath.Join(w.Root, "external")
			if err := os.Mkdir(parent, 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "file")
			if err := os.WriteFile(path, []byte("saved"), 0644); err != nil {
				t.Fatal(err)
			}
			after, _, err := w.capture(saved.ID, path)
			if err != nil {
				t.Fatal(err)
			}
			// Old optional fields are ignored by normal JSON decoding; no
			// historical identity, even a matching one, grants cleanup authority.
			change := map[string]any{
				"path": path, "before": State{}, "after": after, "applied": true,
				"created_dirs": []string{parent},
			}
			if identities {
				info, err := os.Stat(parent)
				if err != nil {
					t.Fatal(err)
				}
				id := directoryID(info)
				change["created_dir_ids"] = map[string]any{parent: map[string]uint64{"device": id.device, "inode": id.inode}}
			}
			raw, err := json.Marshal([]any{change})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Store.CommitChange(saved.ID, intent(t, w, saved, turn, request), json.RawMessage(raw), true); err != nil {
				t.Fatal(err)
			}
			restoreFileTip(t, w, saved.ID, 0)
			assertAbsent(t, path)
			if info, err := os.Stat(parent); err != nil || !info.IsDir() {
				t.Fatalf("historical metadata authorized cleanup: %v", err)
			}
			if len(w.directories.owned) != 0 {
				t.Fatal("historical metadata became runtime ownership")
			}
		})
	}
}

func TestDirectoryOwnershipFollowsManagerNotSession(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-manager", true: "new-manager"}[reopen], func(t *testing.T) {
			w, saved, turn, request := fixture(t)
			for _, op := range []Mutation{{Path: "owned/file", Data: []byte("file")}, {Path: "owned/file", Delete: true}} {
				if _, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{op}); err != nil {
					t.Fatal(err)
				}
			}
			if reopen {
				var err error
				w, err = Open(w.Root, w.Store)
				if err != nil {
					t.Fatal(err)
				}
			}
			selection := llm.Selection{Model: llm.ScriptModel()}
			id := history.NewID("session")
			turn, _, err := w.Store.StartSession(id, w.Root, selection, llm.Message{Type: llm.TypeUserInput, Content: "new session"})
			if err != nil {
				t.Fatal(err)
			}
			saved, err = w.Store.Session(id)
			if err != nil {
				t.Fatal(err)
			}
			request, err = w.Store.StartRequest(id, turn, "main", "coding", selection)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Apply(context.Background(), id, intent(t, w, saved, turn, request), []Mutation{{Path: "owned/new", Data: []byte("new")}}); err != nil {
				t.Fatal(err)
			}
			restoreFileTip(t, w, id, 0)
			parent := filepath.Join(w.Root, "owned")
			if reopen {
				if info, err := os.Stat(parent); err != nil || !info.IsDir() {
					t.Fatalf("new manager acquired earlier ownership: %v", err)
				}
			} else {
				assertAbsent(t, parent)
			}
		})
	}
}

func TestRestoreAttemptSettlesDirectoryEffects(t *testing.T) {
	for _, fault := range []string{"first-write", "second-write", "post-rename"} {
		t.Run(fault, func(t *testing.T) {
			w, saved, turn, request := fixture(t)
			result, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{
				{Path: "a/nested/file", Data: []byte("a")},
				{Path: "z/nested/file", Data: []byte("z")},
			})
			if err != nil {
				t.Fatal(err)
			}
			restoreFileTip(t, w, saved.ID, 0)
			write := w.atomicFile
			failure := errors.New("injected restore failure")
			w.atomicFile = func(path string, data []byte, mode os.FileMode) error {
				if fault == "first-write" {
					return failure
				}
				if path == filepath.Join(w.Root, "z/nested/file") {
					if fault == "post-rename" {
						if err := write(path, data, mode); err != nil {
							return err
						}
					}
					return failure
				}
				return write(path, data, mode)
			}
			err = w.Restore(context.Background(), saved.ID, history.RestoreTarget{FileTip: result.ChangeID})
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
			assertFileTip(t, w, saved.ID, 0)
			wantOwned := 0
			var partial *RestoreError
			if fault == "first-write" {
				assertAbsent(t, filepath.Join(w.Root, "a"))
				if errors.As(err, &partial) {
					t.Fatalf("no file was applied: %+v", partial)
				}
			} else {
				wantOwned = 2
				assertContents(t, filepath.Join(w.Root, "a/nested/file"), "a")
				if !errors.As(err, &partial) || len(partial.Applied) != 1 && fault != "post-rename" {
					t.Fatalf("missing partial effects: %v", err)
				}
			}
			if fault == "post-rename" {
				wantOwned = 4
				assertContents(t, filepath.Join(w.Root, "z/nested/file"), "z")
				if len(partial.Applied) != 2 || len(partial.Remaining) != 0 {
					t.Fatalf("wrong post-rename effects: %+v", partial)
				}
			} else {
				assertAbsent(t, filepath.Join(w.Root, "z"))
			}
			if len(w.directories.owned) != wantOwned {
				t.Fatalf("owned = %+v, want %d directories", w.directories.owned, wantOwned)
			}
		})
	}
}
