package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"ttc/internal/history"
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
		if len(w.ownedDirs) != 0 {
			t.Fatalf("removed directory identities retained: %+v", w.ownedDirs)
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
	for _, op := range []Mutation{{Path: "owned/file", Data: []byte("file")}, {Path: "owned/file", Delete: true}} {
		if _, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{op}); err != nil {
			t.Fatal(err)
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
}
