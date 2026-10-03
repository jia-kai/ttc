package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestWorkspaceGitParentsNestedWorktreeAndHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git optional")
	}
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(b))
		}
		return strings.TrimSpace(string(b))
	}
	git(root, "init", "-b", "main")
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	info := inspectWorkspace(context.Background(), nested)
	if info.cwd != nested || info.repo != root || info.branch != "main" {
		t.Fatal(info)
	}
	git(root, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	git(root, "checkout", "--detach")
	if v := inspectWorkspace(context.Background(), nested); v.branch != "detached" {
		t.Fatal(v)
	}
	git(root, "checkout", "main")
	worktree := filepath.Join(t.TempDir(), "worktree")
	git(root, "worktree", "add", "-b", "research", worktree)
	if v := inspectWorkspace(context.Background(), worktree); v.repo != worktree || v.branch != "research" {
		t.Fatal(v)
	}
	git(nested, "init", "-b", "inner")
	if v := inspectWorkspace(context.Background(), nested); v.repo != nested || v.branch != "inner" {
		t.Fatal(v)
	}
	if v := inspectWorkspace(context.Background(), t.TempDir()); v.repo != "" || v.branch != "" {
		t.Fatal(v)
	}
}

func TestWorkspaceMissingGitTimeoutAndWorkerClose(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if v := inspectWorkspace(context.Background(), bin); v.repo != "" || v.gitError == "" {
		t.Fatal(v)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n/bin/sleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if v := inspectWorkspace(ctx, bin); v.repo != "" || v.gitError == "" {
		t.Fatal(v)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Git timeout blocked")
	}
	m := newWorkspaceMonitor(context.Background(), bin)
	start = time.Now()
	m.close()
	if time.Since(start) > time.Second {
		t.Fatal("worker did not join promptly")
	}
}

func TestSidebarWorkspaceLongUnicodePathsAndClick(t *testing.T) {
	b := newSidebar()
	path := "/long/" + strings.Repeat("研究/", 30) + "project"
	b.workspace = workspaceInfo{cwd: path, repo: "/long/repository", branch: "research"}
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(80, 20)
	b.overlay = true
	b.bounds(80, 20, false)
	at := time.Now()
	b.drawAt(s, at)
	if b.workspaceHeight != 5 {
		t.Fatal(b.workspaceHeight)
	}
	text := b.texts["workspace:cwd"]
	b.drawAt(s, at.Add(time.Duration(4+text.cells-(b.width-6))*250*time.Millisecond))
	if got := sidebarScreenText(s, b.left+5, 1, b.width-6); !strings.HasSuffix(got, "project") {
		t.Fatal("workspace path tail never visible", got)
	}
	consumed, action := b.mouse(tcell.NewEventMouse(b.left+3, 1, tcell.Button1, 0))
	if !consumed || !action.workspace || !strings.Contains(b.workspace.detail(), path) {
		t.Fatal("full workspace detail unavailable")
	}
	b.mouse(tcell.NewEventMouse(b.left+3, b.sections[1].top, tcell.Button1, 0))
	if !b.sections[1].collapsed || b.sections[0].collapsed {
		t.Fatal("workspace header shifted list hit testing")
	}
}
