package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/history"
	"ttc/internal/historytest"
)

func TestMutationTargetConflictsPreflight(t *testing.T) {
	for _, names := range [][]string{{"parent", "parent/child"}, {"parent/child", "parent"}, {"parent/child", "./parent"}, {"same", "./same"}} {
		t.Run(strings.Join(names, "-"), func(t *testing.T) {
			w, saved, turn, request := fixture(t)
			transforms := 0
			var ops []Mutation
			for _, name := range names {
				ops = append(ops, Mutation{Path: name, Transform: func([]byte) ([]byte, error) {
					transforms++
					return []byte("new"), nil
				}})
			}
			result, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), ops)
			if err == nil || result.ChangeID != 0 || result.HistoryRecorded || transforms != 0 {
				t.Fatalf("conflict was not preflighted: %+v, %v, transforms=%d", result, err, transforms)
			}
			entries, err := os.ReadDir(w.Root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("preflight changed workspace: %v, %v", entries, err)
			}
			assertFileTip(t, w, saved.ID, 0)
		})
	}
	w, saved, turn, request := fixture(t)
	if _, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{
		{Path: "parent", Data: []byte("one")}, {Path: "parent-sibling/child", Data: []byte("two")},
	}); err != nil {
		t.Fatalf("lexical prefix is not an ancestor: %v", err)
	}
}

func assertFileTip(t *testing.T, w *Manager, session string, want int64) {
	t.Helper()
	saved, err := w.Store.Session(session)
	if err != nil || saved.FileTip != want {
		t.Fatalf("file tip = %d, want %d: %v", saved.FileTip, want, err)
	}
}

func assertContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("%s = %q, want %q: %v", path, got, want, err)
	}
}

func TestCreatedDirectoryCleanupOwnership(t *testing.T) {
	for _, replacement := range []string{"none", "file", "symlink", "directory"} {
		t.Run(replacement, func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "new")
			created, err := createDirs(filepath.Join(parent, "nested", "file"))
			if err != nil {
				t.Fatal(err)
			}
			if len(created) != 2 {
				t.Fatalf("actual creation was not tracked: %+v", created)
			}
			if replacement != "none" {
				// Keep the old inode alive so an inode cannot be immediately reused.
				if err := os.Rename(parent, filepath.Join(root, "original")); err != nil {
					t.Fatal(err)
				}
				switch replacement {
				case "file":
					if err := os.WriteFile(parent, []byte("external"), 0600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink(filepath.Join(root, "original"), parent); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(parent, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			var ownership directoryOwnership
			ownership.finish(mutationEffect{created: created})
			if len(ownership.owned) != 0 {
				t.Fatalf("removed or replaced identities retained: %+v", ownership.owned)
			}
			_, err = os.Lstat(parent)
			if replacement == "none" {
				if !os.IsNotExist(err) {
					t.Fatalf("owned empty directory remains: %v", err)
				}
			} else if err != nil {
				t.Fatalf("cleanup removed unverified replacement: %v", err)
			}
			if replacement == "file" {
				assertContents(t, parent, "external")
			}
			if replacement == "symlink" {
				if _, err := os.Stat(filepath.Join(root, "original", "nested")); err != nil {
					t.Fatal("cleanup followed a substituted ancestor symlink", err)
				}
			}
		})
	}
}

func TestFailureCleanupPreservesSubstitutedRegularFile(t *testing.T) {
	for _, stage := range []string{"preflight", "created"} {
		t.Run(stage, func(t *testing.T) {
			w, saved, turn, request := fixture(t)
			parent := filepath.Join(w.Root, "parent")
			ops := []Mutation{{Path: "saved", Data: []byte("applied")}, {Path: "parent/child", Data: []byte("not applied")}}
			if stage == "preflight" {
				ops[1].Transform = func([]byte) ([]byte, error) {
					return []byte("not applied"), os.WriteFile(parent, []byte("external"), 0600)
				}
			} else {
				write := w.atomicFile
				w.atomicFile = func(path string, data []byte, mode os.FileMode) error {
					if filepath.Base(path) != "child" {
						return write(path, data, mode)
					}
					if err := os.Rename(parent, filepath.Join(w.Root, "old-directory")); err != nil {
						return err
					}
					if err := os.WriteFile(parent, []byte("external"), 0600); err != nil {
						return err
					}
					return errors.New("injected replacement before write")
				}
			}
			result, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), ops)
			if err == nil || !result.HistoryRecorded || !result.Changes[0].Applied || result.Changes[1].Applied {
				t.Fatalf("incorrect partial outcome: %+v, %v", result, err)
			}
			assertContents(t, parent, "external")
			assertContents(t, filepath.Join(w.Root, "saved"), "applied")
			if err := current(result.Changes[0].Path, result.Changes[0].After); err != nil {
				t.Fatal("cleanup invalidated applied snapshot", err)
			}
		})
	}
}

func TestDirectoryCreationDoesNotClaimExistingParents(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "external")
	path := filepath.Join(parent, "file")
	if len(missingDirs(path)) != 1 {
		t.Fatal("parent should initially be absent")
	}
	if err := os.Mkdir(parent, 0755); err != nil {
		t.Fatal(err)
	}
	created, err := createDirs(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("claimed external creator's directory: %+v", created)
	}
	var ownership directoryOwnership
	ownership.finish(mutationEffect{created: created})
	if _, err := os.Stat(parent); err != nil {
		t.Fatal("removed external parent", err)
	}
}

func TestMutationFailureCleanupAndAppliedSnapshots(t *testing.T) {
	w, saved, turn, request := fixture(t)
	write := w.atomicFile
	failure := errors.New("injected write failure")
	w.atomicFile = func(path string, data []byte, mode os.FileMode) error {
		if strings.HasSuffix(path, "failed") {
			return failure
		}
		return write(path, data, mode)
	}
	result, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{
		{Path: "shared/saved", Data: []byte("saved")},
		{Path: "shared/new/nested/failed", Data: []byte("failed")},
		{Path: "never/reached", Data: []byte("never")},
	})
	if !errors.Is(err, failure) || !result.HistoryRecorded || result.ChangeID == 0 {
		t.Fatalf("partial result: %+v, %v", result, err)
	}
	if len(result.Changes) != 3 || !result.Changes[0].Applied || result.Changes[1].Applied || result.Changes[2].Applied {
		t.Fatalf("wrong applied subset: %+v", result)
	}
	for _, change := range result.Changes {
		state := change.Before
		if change.Applied {
			state = change.After
		}
		if err := current(change.Path, state); err != nil {
			t.Fatalf("snapshot disagrees with disk: %+v: %v", change, err)
		}
	}
	for _, name := range []string{"shared/new", "never"} {
		if _, err := os.Stat(filepath.Join(w.Root, name)); !os.IsNotExist(err) {
			t.Fatalf("unapplied directory survived: %s: %v", name, err)
		}
	}
	if len(w.directories.owned) != 1 {
		t.Fatalf("failed attempt retained directory authority: %+v", w.directories.owned)
	}
	assertFileTip(t, w, saved.ID, result.ChangeID)
	changes, err := w.Store.Changes(result.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	var recorded []PathChange
	if err := json.Unmarshal(changes[0].Paths, &recorded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recorded, result.Changes) {
		t.Fatalf("recorded subset differs: %+v / %+v", recorded, result.Changes)
	}
}

func TestMutationPostRenameFailureAndHistoryFailure(t *testing.T) {
	for _, failHistory := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded", true: "unrecorded"}[failHistory], func(t *testing.T) {
			w, saved, turn, request := fixture(t)
			if failHistory {
				_, err := historytest.DB(t, w.Store).Exec(`CREATE TRIGGER fail_history BEFORE UPDATE OF file_tip_id ON sessions BEGIN SELECT RAISE(ABORT, 'injected history failure'); END`)
				if err != nil {
					t.Fatal(err)
				}
			}
			write := w.atomicFile
			failure := errors.New("injected fsync after rename failure")
			w.atomicFile = func(path string, data []byte, mode os.FileMode) error {
				if err := write(path, data, mode); err != nil {
					return err
				}
				return failure
			}
			result, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{
				{Path: "saved", Data: []byte("written")}, {Path: "remaining", Data: []byte("not written")},
			})
			if !errors.Is(err, failure) || result.HistoryRecorded == failHistory || !result.Changes[0].Applied || result.Changes[1].Applied {
				t.Fatalf("post-rename result: %+v, %v", result, err)
			}
			if failHistory && (result.ChangeID != 0 || !strings.Contains(err.Error(), "injected history failure")) {
				t.Fatalf("lost combined failure or exposed rolled-back ID: %+v, %v", result, err)
			}
			assertContents(t, filepath.Join(w.Root, "saved"), "written")
			assertFileTip(t, w, saved.ID, result.ChangeID)
		})
	}
}

func TestRestorePreflightsAllSnapshotIntegrity(t *testing.T) {
	for _, damage := range []string{"missing", "corrupt", "directory", "oversized", "symlink"} {
		t.Run(damage, func(t *testing.T) {
			w, saved, turn, request := fixture(t)
			for _, name := range []string{"a", "b"} {
				if err := os.WriteFile(filepath.Join(w.Root, name), []byte("before-"+name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{
				{Path: "a", Data: []byte("after-a")}, {Path: "b", Data: []byte("after-b")},
			})
			if err != nil {
				t.Fatal(err)
			}
			blob := result.Changes[1].Before.Blob
			if err := os.Remove(blob); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "corrupt":
				err = os.WriteFile(blob, []byte("corrupt"), 0600)
			case "directory":
				err = os.Mkdir(blob, 0700)
			case "oversized":
				err = os.WriteFile(blob, nil, 0600)
				if err == nil {
					err = os.Truncate(blob, MaxFileBytes+1)
				}
			case "symlink":
				err = os.Symlink(result.Changes[0].Before.Blob, blob)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Restore(context.Background(), saved.ID, history.RestoreTarget{}); err == nil || !strings.Contains(err.Error(), filepath.Join(w.Root, "b")) {
				t.Fatalf("missing precise preflight error: %v", err)
			}
			assertContents(t, filepath.Join(w.Root, "a"), "after-a")
			assertContents(t, filepath.Join(w.Root, "b"), "after-b")
			assertFileTip(t, w, saved.ID, result.ChangeID)
		})
	}
}

func TestRestoreUnexpectedFailuresReportAppliedPaths(t *testing.T) {
	for _, fault := range []string{"cancel", "write", "post-rename", "history"} {
		t.Run(fault, func(t *testing.T) {
			w, saved, turn, request := fixture(t)
			for _, name := range []string{"a", "b"} {
				if err := os.WriteFile(filepath.Join(w.Root, name), []byte("before-"+name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{
				{Path: "a", Data: []byte("after-a")}, {Path: "b", Data: []byte("after-b")},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			write := w.atomicFile
			failure := errors.New("injected restore write failure")
			w.atomicFile = func(path string, data []byte, mode os.FileMode) error {
				if fault == "write" && filepath.Base(path) == "b" {
					return failure
				}
				if err := write(path, data, mode); err != nil {
					return err
				}
				if fault == "cancel" {
					cancel()
				}
				if fault == "post-rename" && filepath.Base(path) == "b" {
					return failure
				}
				return nil
			}
			if fault == "history" {
				if _, err := historytest.DB(t, w.Store).Exec(`CREATE TRIGGER fail_restore BEFORE UPDATE OF file_tip_id ON sessions BEGIN SELECT RAISE(ABORT, 'injected restore history failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err = w.Restore(ctx, saved.ID, history.RestoreTarget{})
			var partial *RestoreError
			if !errors.As(err, &partial) {
				t.Fatalf("no recovery manifest: %v", err)
			}
			want := []string{filepath.Join(w.Root, "a")}
			remaining := []string{filepath.Join(w.Root, "b")}
			if fault == "post-rename" || fault == "history" {
				want = append(want, remaining...)
				remaining = []string{}
			}
			if !reflect.DeepEqual(partial.Applied, want) || !reflect.DeepEqual(partial.Remaining, remaining) {
				t.Fatalf("incorrect recovery manifest: %+v", partial)
			}
			if fault == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			for _, path := range want {
				assertContents(t, path, "before-"+filepath.Base(path))
			}
			for _, path := range remaining {
				assertContents(t, path, "after-"+filepath.Base(path))
			}
			assertFileTip(t, w, saved.ID, result.ChangeID)
		})
	}
}
