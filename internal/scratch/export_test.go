package scratch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWriteExportAtSafety(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "inside")); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(root, "existing")
	if err := os.WriteFile(existing, []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"relative", root, outside + "/target", root + "/../target", root + "/missing/target",
		root + "/escape/target", root + "/inside/target", root + "/link", existing,
	} {
		t.Run(path, func(t *testing.T) {
			called := false
			_, err := writeExportAt(context.Background(), root, path, func(w io.Writer) error { called = true; _, e := io.WriteString(w, "new"); return e })
			if err == nil || called {
				t.Fatalf("err=%v callback=%v", err, called)
			}
		})
	}
	if b, err := os.ReadFile(existing); err != nil || string(b) != "preserved" {
		t.Fatalf("existing changed: %q %v", b, err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside changed: %v %v", entries, err)
	}
}

func TestWriteExportAtExclusiveAndPrivate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "export")
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			n, err := writeExportAt(context.Background(), root, path, func(w io.Writer) error { _, e := io.WriteString(w, "content"); return e })
			if err == nil && n != 7 {
				t.Errorf("bytes=%d", n)
			}
			results <- err
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful creators=%d", successes)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", st.Mode().Perm())
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "content" {
		t.Fatalf("content=%q err=%v", b, err)
	}
}

func TestWriteExportAtFailureAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "cancel"}[cancel], func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "export")
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			failure := errors.New("fixture failure")
			n, err := writeExportAt(ctx, root, path, func(w io.Writer) error {
				if _, err := io.WriteString(w, "partial"); err != nil {
					return err
				}
				if cancel {
					stop()
					_, err := io.WriteString(w, strings.Repeat("x", 1<<20))
					return err
				}
				return failure
			})
			want := failure
			if cancel {
				want = context.Canceled
			}
			if !errors.Is(err, want) || n != 7 {
				t.Fatalf("bytes=%d err=%v", n, err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("partial remains: %v", err)
			}
		})
	}
}

func TestWriteExportAtCancellationAfterCallback(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "export")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := writeExportAt(ctx, root, path, func(w io.Writer) error {
		if _, err := io.WriteString(w, "partial"); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("partial remains: %v", err)
	}
}

func TestWriteExportAtCleanupPreservesReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "export")
	_, err := writeExportAt(context.Background(), root, path, func(w io.Writer) error {
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
			return err
		}
		return errors.New("fixture failure")
	})
	if err == nil {
		t.Fatal("missing failure")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "replacement" {
		t.Fatalf("replacement=%q err=%v", b, err)
	}
}

func TestWriteExportAtPinsParent(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "moved")
	outside := t.TempDir()
	_, err := writeExportAt(context.Background(), root, filepath.Join(parent, "export"), func(w io.Writer) error {
		if err := os.Rename(parent, moved); err != nil {
			return err
		}
		if err := os.Symlink(outside, parent); err != nil {
			return err
		}
		return errors.New("fixture failure")
	})
	if err == nil {
		t.Fatal("missing failure")
	}
	if entries, err := os.ReadDir(moved); err != nil || len(entries) != 0 {
		t.Fatalf("partial remains: %v %v", entries, err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside changed: %v %v", entries, err)
	}
}
