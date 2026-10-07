package scratch

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ttc/internal/prompts"
)

func TestDiagnosticAssetsExact(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{prompts.ScratchOwnedRequirement, "owned 0700 directory"},
		{prompts.ScratchSharedRequirement, "shared sticky 1777 directory"},
		{prompts.ScratchSetPermissions, "set scratch permissions %s: %w"},
		{prompts.ScratchCreate, "create scratch %s: %w"},
		{prompts.ScratchInspect, "inspect scratch %s: %w"},
		{prompts.ScratchUnsafeDirectory, "unsafe scratch directory %s: require %s"},
	} {
		if tc.got != tc.want {
			t.Fatalf("diagnostic changed: %q, want %q", tc.got, tc.want)
		}
	}
}

func TestRuntimeDiagnosticsExact(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scratch")
	path, err := verifyAt(root, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	_, err = verifyAt(root, os.Geteuid())
	if want := "unsafe scratch directory " + path + ": require owned 0700 directory"; err == nil || err.Error() != want {
		t.Fatalf("private requirement: %v, want %q", err, want)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = verifyAt(root, os.Geteuid())
	if want := "unsafe scratch directory " + root + ": require shared sticky 1777 directory"; err == nil || err.Error() != want {
		t.Fatalf("shared requirement: %v, want %q", err, want)
	}
	root = filepath.Join(t.TempDir(), "missing", "scratch")
	_, err = verifyAt(root, os.Geteuid())
	var cause *os.PathError
	if !errors.As(err, &cause) || !errors.Is(err, os.ErrNotExist) || err.Error() != "create scratch "+root+": "+cause.Error() {
		t.Fatalf("create wrapping changed: %v", err)
	}
}

func TestPrivateScratchRecreatedAndUnsafeRejected(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scratch")
	p, e := verifyAt(root, os.Geteuid())
	if e != nil {
		t.Fatal(e)
	}
	for path, want := range map[string]os.FileMode{root: os.ModeSticky | 0777, p: 0700} {
		st, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode() & (os.ModePerm | os.ModeSticky); got != want {
			t.Fatalf("%s: mode %v, want %v", path, got, want)
		}
	}
	if e = os.Remove(p); e != nil {
		t.Fatal(e)
	}
	if _, e = verifyAt(root, os.Geteuid()); e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(p, 0755); e != nil {
		t.Fatal(e)
	}
	if _, e = verifyAt(root, os.Geteuid()); e == nil {
		t.Fatal("accepted public directory")
	}
	if e = os.Remove(p); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(t.TempDir(), p); e != nil {
		t.Fatal(e)
	}
	if _, e = verifyAt(root, os.Geteuid()); e == nil {
		t.Fatal("accepted symlink")
	}
}

func TestSharedScratchRejectsUnsafeRoot(t *testing.T) {
	for _, mode := range []os.FileMode{0700, 0777, os.ModeSticky | 0755, os.ModeSticky | os.ModeSetgid | 0777} {
		t.Run(mode.String(), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "scratch")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyAt(root, os.Geteuid()); err == nil {
				t.Fatal("accepted unsafe shared root")
			}
		})
	}
	root := filepath.Join(t.TempDir(), "scratch")
	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyAt(root, os.Geteuid()); err == nil {
		t.Fatal("accepted shared root symlink")
	}
}

func TestScratchRejectsChildOwnedByAnotherUser(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scratch")
	if _, err := verifyAt(root, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	otherUID := os.Geteuid() + 1
	otherPath := filepath.Join(root, strconv.Itoa(otherUID))
	if err := os.Mkdir(otherPath, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyAt(root, otherUID); err == nil {
		t.Fatal("accepted child owned by another UID")
	} else if !strings.Contains(err.Error(), otherPath) {
		// The shared root belongs to a different UID than the requested child;
		// only the private child must fail ownership validation.
		t.Fatalf("rejected shared root ownership instead of private child: %v", err)
	}
}
