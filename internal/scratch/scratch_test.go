package scratch

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

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
