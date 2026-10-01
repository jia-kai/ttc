package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestInstructionFileBoundaries(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(regular, []byte("regular instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.md")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	b, err := ReadInstruction(context.Background(), link)
	if err != nil || string(b) != "regular instructions" {
		t.Fatal(string(b), err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	fifoLink := filepath.Join(dir, "fifo-link")
	if err := os.Symlink(fifo, fifoLink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, fifoLink, dir} {
		if _, err := ReadInstruction(context.Background(), path); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("special file %s: %v", path, err)
		}
	}
	if err := os.WriteFile(regular, []byte(strings.Repeat("x", maxInstructionBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstruction(context.Background(), regular); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatal("oversized file accepted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadInstruction(ctx, regular); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not honored", err)
	}
}

func TestSkillDiscoveryAndReloadRejectSpecialFiles(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, ".agents", "skills", "unsafe", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(context.Background(), project, ""); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatal("FIFO skill accepted", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := Discover(context.Background(), project, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Load(context.Background(), "unsafe"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatal("unsafe replacement accepted", err)
	}
}
