package privatefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateDir(t *testing.T) {
	t.Run("create and reuse", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested", "private")
		for range 2 {
			if err := PrivateDir(path); err != nil {
				t.Fatal(err)
			}
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatalf("private directory: %v, %v", info, err)
		}
	})
	for _, mode := range []os.FileMode{0755, 0770, 0600} {
		t.Run(mode.String(), func(t *testing.T) {
			path := t.TempDir()
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if err := PrivateDir(path); err == nil {
				t.Fatal("accepted unsafe directory mode")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("changed existing mode: %v, %v", info, err)
			}
		})
	}
	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := PrivateDir(path); err == nil {
			t.Fatal("accepted regular file")
		}
		assertFile(t, path, "keep", 0600)
	})
	t.Run("final symlink", func(t *testing.T) {
		target := t.TempDir()
		path := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if err := PrivateDir(path); err == nil {
			t.Fatal("accepted final symlink")
		}
		if actual, err := os.Readlink(path); err != nil || actual != target {
			t.Fatalf("changed symlink: %q, %v", actual, err)
		}
	})
	t.Run("ancestor policy belongs to caller", func(t *testing.T) {
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := PrivateDir(filepath.Join(link, "private")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAtomicFileReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	if err := AtomicFile(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "first", 0600)
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := AtomicFile(path, []byte("second"), 0640); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "second", 0640)
	data := make([]byte, 5)
	if n, err := old.Read(data); err != nil || n != 5 || string(data) != "first" {
		t.Fatalf("replacement mutated old inode: %q, %v", data, err)
	}
	if err := SyncDir(dir); err != nil {
		t.Fatalf("sync published directory: %v", err)
	}
	assertEntries(t, dir, "state")
}

func TestAtomicFileReplacesSymlinkWithoutChangingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := AtomicFile(path, []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "inside", 0600)
	assertFile(t, target, "outside", 0600)
	assertEntries(t, dir, "state")
}

func TestAtomicFileFailuresLeaveNoTemporaryFiles(t *testing.T) {
	t.Run("missing parent", func(t *testing.T) {
		dir := t.TempDir()
		if err := AtomicFile(filepath.Join(dir, "missing", "state"), []byte("new"), 0600); err == nil {
			t.Fatal("accepted missing parent")
		}
		assertEntries(t, dir)
	})
	t.Run("non-directory parent", func(t *testing.T) {
		dir := t.TempDir()
		parent := filepath.Join(dir, "parent")
		if err := os.WriteFile(parent, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := AtomicFile(filepath.Join(parent, "state"), []byte("new"), 0600); err == nil {
			t.Fatal("accepted non-directory parent")
		}
		assertFile(t, parent, "keep", 0600)
		assertEntries(t, dir, "parent")
	})
	t.Run("rename failure", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state")
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(path, "keep")
		if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := AtomicFile(path, []byte("new"), 0600); err == nil {
			t.Fatal("replaced directory with file")
		}
		assertFile(t, marker, "keep", 0600)
		assertEntries(t, dir, "state")
	})
}

func TestSyncDirMissing(t *testing.T) {
	if err := SyncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("synced missing directory")
	}
}

func assertFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		t.Fatalf("%s content: %q, %v", path, data, err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		t.Fatalf("%s mode: %v, %v", path, info, err)
	}
}

func assertEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Fatalf("unexpected entries (temporary file leaked): %v; want %v", entries, want)
	}
	for i, name := range want {
		if entries[i].Name() != name {
			t.Fatalf("entry %q, want %q", entries[i].Name(), name)
		}
	}
}
