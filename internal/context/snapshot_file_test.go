package context

import (
	stdcontext "context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSnapshotRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Snapshot(stdcontext.Background(), path, nil); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file or directory") {
			t.Fatal("FIFO accepted", err)
		}
	case <-time.After(time.Second):
		// Rescue a blocking open so a regression cannot hang the test process.
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = f.Close()
		}
		t.Fatal("attachment waited for a FIFO writer")
	}
}

func TestSnapshotUsesOpenedFileAfterPathReplacement(t *testing.T) {
	for _, directory := range []bool{false, true} {
		name := "file"
		if directory {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "attachment")
			if directory {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "child.txt"), []byte("text"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(path, "nested"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "nested", "deep.txt"), []byte("text"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := snapshotOpened(stdcontext.Background(), path, f, nil)
			want := "original"
			if directory {
				want = "child.txt\nnested\nnested/deep.txt"
			}
			if err != nil || got.Text != want {
				t.Fatalf("opened attachment changed: %+v, %v", got, err)
			}
		})
	}
}

func TestSnapshotExplicitFileSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "text")
	if err := os.WriteFile(path, []byte("linked text"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, path+".link"); err != nil {
		t.Fatal(err)
	}
	got, err := Snapshot(stdcontext.Background(), path+".link", nil)
	if err != nil || got.Text != "linked text" {
		t.Fatal("explicit file symlink rejected", got, err)
	}
}
