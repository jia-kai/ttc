package rail

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHostInspectionUnchanged(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("file", link); err != nil {
		t.Fatal(err)
	}
	var h hostInspection
	for range 2 {
		for _, path := range []string{file, dir, link} {
			if _, err := h.lstat(ctx, path); err != nil {
				t.Fatal(err)
			}
			if _, err := h.stat(ctx, path); err != nil {
				t.Fatal(err)
			}
		}
		if target, err := h.readlink(ctx, link); err != nil || target != "file" {
			t.Fatalf("readlink = %q, %v", target, err)
		}
	}
	p := sandboxPlan{hostObservations: h.freeze()}
	if err := p.validateHost(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHostInspectionChanges(t *testing.T) {
	for _, name := range []string{"replacement", "mode", "content", "mtime", "ctime", "nlink", "removal"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			var h hostInspection
			info, err := h.stat(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			p := sandboxPlan{hostObservations: h.freeze()}
			switch name {
			case "replacement":
				// Keep the old inode alive, avoiding inode reuse in this assertion.
				err = os.Rename(path, path+".old")
				if err == nil {
					err = os.WriteFile(path, []byte("original"), 0600)
				}
			case "mode":
				err = os.Chmod(path, 0640)
			case "content":
				err = os.WriteFile(path, []byte("different length"), 0600)
			case "mtime":
				err = os.Chtimes(path, info.ModTime(), info.ModTime().Add(time.Hour))
			case "ctime":
				// Preserve content size, mode and mtime; ctime is the remaining signal.
				err = os.WriteFile(path, []byte("modified"), 0600)
				if err == nil {
					err = os.Chtimes(path, info.ModTime(), info.ModTime())
				}
			case "nlink":
				err = os.Link(path, path+".hardlink")
			case "removal":
				err = os.Remove(path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := p.validateHost(ctx); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("validation error = %v; want changed path", err)
			}
			if _, err := h.stat(ctx, path); err == nil || errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("repeated stat error = %v; change must not be optional absence", err)
			}
		})
	}
}

func TestHostInspectionSymlinkRetarget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file, alias, link := filepath.Join(dir, "file"), filepath.Join(dir, "alias"), filepath.Join(dir, "link")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(file, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", link); err != nil {
		t.Fatal(err)
	}
	var h hostInspection
	// Lstat alone captures the literal target, even when Stat would see the same inode.
	if _, err := h.lstat(ctx, link); err != nil {
		t.Fatal(err)
	}
	p := sandboxPlan{hostObservations: h.freeze()}
	if err := os.Rename(link, link+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("alias", link); err != nil {
		t.Fatal(err)
	}
	if err := p.validateHost(ctx); err == nil {
		t.Fatal("retargeted symlink validated")
	}
	if _, err := h.readlink(ctx, link); err == nil {
		t.Fatal("retargeted repeated readlink accepted")
	}
}

func TestHostInspectionAbsence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "optional")
	var h hostInspection
	for range 2 {
		if _, err := h.lstat(ctx, path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing lstat = %v", err)
		}
		if _, err := h.stat(ctx, path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing stat = %v", err)
		}
	}
	p := sandboxPlan{hostObservations: h.freeze()}
	if err := p.validateHost(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.validateHost(ctx); err == nil {
		t.Fatal("appearance of optional import accepted")
	}
	if _, err := h.lstat(ctx, path); err == nil {
		t.Fatal("inconsistent repeated absence accepted")
	}
}

func TestHostInspectionCrossChecksStatAndLstat(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var h hostInspection
	if _, err := h.lstat(ctx, path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.stat(ctx, path); err == nil {
		t.Fatal("different probe forms accepted inconsistent metadata")
	}
}

func TestHostInspectionDirectoryChildChanges(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var h hostInspection
	if _, err := h.stat(ctx, dir); err != nil {
		t.Fatal(err)
	}
	p := sandboxPlan{hostObservations: h.freeze()}
	file := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(file, []byte("set -g status off\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.stat(ctx, dir); err != nil {
		t.Fatalf("directory child activity rejected: %v", err)
	}
	if err := p.validateHost(ctx); err != nil {
		t.Fatalf("directory child activity invalidated plan: %v", err)
	}
}

func TestHostInspectionDirectorySubdirectoryChanges(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var h hostInspection
	if _, err := h.lstat(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := h.stat(ctx, dir); err != nil {
		t.Fatal(err)
	}
	p := sandboxPlan{hostObservations: h.freeze()}
	for _, observation := range p.hostObservations {
		if observation.fingerprint.nlink != 0 {
			t.Fatal("directory observation retained child-sensitive link count")
		}
	}
	child := filepath.Join(dir, "unrelated-child")
	for _, step := range []struct {
		name   string
		change func() error
	}{
		{"create", func() error { return os.Mkdir(child, 0700) }},
		{"remove", func() error { return os.Remove(child) }},
	} {
		t.Run(step.name, func(t *testing.T) {
			if err := step.change(); err != nil {
				t.Fatal(err)
			}
			if _, err := h.lstat(ctx, dir); err != nil {
				t.Fatalf("directory Lstat rejected child change: %v", err)
			}
			if _, err := h.stat(ctx, dir); err != nil {
				t.Fatalf("directory Stat rejected child change: %v", err)
			}
			if err := p.validateHost(ctx); err != nil {
				t.Fatalf("directory child change invalidated plan: %v", err)
			}
		})
	}
}

func TestHostInspectionFrozenOwnershipAndOrder(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var h hostInspection
	for _, name := range []string{"z", "a"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := h.stat(ctx, path); err != nil {
			t.Fatal(err)
		}
		if _, err := h.lstat(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	frozen := h.freeze()
	if len(frozen) != 4 || frozen[0].key.path != filepath.Join(dir, "a") || frozen[0].key.follow || !frozen[1].key.follow || frozen[2].key.path != filepath.Join(dir, "z") {
		t.Fatalf("unexpected observation order: %+v", frozen)
	}
	if _, err := h.lstat(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if len(frozen) != 4 || len(h.freeze()) != 5 {
		t.Fatal("freeze did not isolate later observations")
	}
	frozen[0].key.path = "not an observed path"
	frozen[1].fingerprint.exists = false
	p := sandboxPlan{hostObservations: h.freeze()}
	if err := p.validateHost(ctx); err != nil {
		t.Fatalf("mutating frozen copy changed observer: %v", err)
	}
}

func TestHostInspectionUnexpectedErrors(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var h hostInspection
	if _, err := h.stat(ctx, filepath.Join(path, "child")); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unexpected IO treated as absence: %v", err)
	}
	if len(h.freeze()) != 0 {
		t.Fatal("failed IO became an observation")
	}
	if _, err := h.readlink(ctx, path); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("readlink of regular file = %v", err)
	}
}

func TestHostInspectionCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var h hostInspection
	if _, err := h.lstat(ctx, "unused"); !errors.Is(err, context.Canceled) {
		t.Fatalf("lstat cancellation = %v", err)
	}
	if _, err := h.stat(ctx, "unused"); !errors.Is(err, context.Canceled) {
		t.Fatalf("stat cancellation = %v", err)
	}
	if _, err := h.readlink(ctx, "unused"); !errors.Is(err, context.Canceled) {
		t.Fatalf("readlink cancellation = %v", err)
	}
	if err := (&sandboxPlan{}).validateHost(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("empty plan cancellation = %v", err)
	}
	if len(h.freeze()) != 0 {
		t.Fatal("canceled call recorded an observation")
	}
	path := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink("target", path); err != nil {
		t.Fatal(err)
	}
	for _, after := range []int{2, 3} {
		// Err checks occur before Lstat, after Lstat, then after Readlink.
		ctx := &cancelAfterHostChecks{Context: context.Background(), after: after}
		if _, err := h.lstat(ctx, path); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation at check %d = %v", after, err)
		}
		if len(h.freeze()) != 0 {
			t.Fatal("partially canceled observation recorded")
		}
	}
}

type cancelAfterHostChecks struct {
	context.Context
	after, checks int
}

func (c *cancelAfterHostChecks) Err() error {
	c.checks++
	if c.checks >= c.after {
		return context.Canceled
	}
	return nil
}
