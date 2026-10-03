package rail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"scicode/internal/filelock"
)

func TestInstanceMetadataSizeLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.json")
	data, err := json.Marshal(instance{Version: 1, Workdir: "/fixture/project"})
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, bytes.Repeat([]byte(" "), maxInstanceBytes-len(data))...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if info, err := readInstance(dir); err != nil || info.Workdir != "/fixture/project" {
		t.Fatalf("exact 64 KiB metadata rejected: %+v %v", info, err)
	}
	if err := os.WriteFile(path, append(data, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readInstance(dir); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("64 KiB + 1 metadata accepted: %v", err)
	}
	// A large sparse record exercises bounded input without a large fixture.
	if err := os.Truncate(path, 64<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := readInstance(dir); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("large sparse metadata accepted: %v", err)
	}
}

func TestInstanceMetadataRejectsNonregularEntries(t *testing.T) {
	for _, kind := range []string{"directory", "fifo", "symlink", "dangling symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "instance.json")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "symlink":
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(target, []byte(`{"version":1,"workdir":"/fixture"}`), 0600); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(target, path)
			case "dangling symlink":
				err = os.Symlink(filepath.Join(dir, "missing"), path)
			}
			if err != nil {
				t.Fatal(err)
			}
			// O_NONBLOCK permits rejecting a FIFO without waiting for a writer;
			// O_NOFOLLOW rejects both existing and dangling symlink entrypoints.
			if _, err := readInstance(dir); err == nil {
				t.Fatalf("accepted %s metadata", kind)
			}
		})
	}
}

func TestListRejectsInvalidMetadataAndHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, workdirKey("/fixture/project"))
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	lock, err := filelock.Acquire(context.Background(), filepath.Join(dir, "server.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Mkfifo(filepath.Join(dir, "instance.json"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := listInstances(context.Background(), root, &out); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("listing did not reject invalid metadata: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := listInstances(ctx, root, &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("listing ignored cancellation: %v", err)
	}
}
