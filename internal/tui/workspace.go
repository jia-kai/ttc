package tui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type workspaceInfo struct{ cwd, repo, branch, gitError string }

func (v workspaceInfo) detail() string {
	text := "Working directory:\n" + v.cwd
	if v.repo != "" {
		text += "\n\nGit repository:\n" + v.repo
		if v.branch != "" {
			text += "\n\nBranch:\n" + v.branch
		}
	} else {
		text += "\n\nGit: no repository available"
	}
	if v.gitError != "" {
		text += "\n" + v.gitError
	}
	return text
}

// workspaceMonitor owns periodic Git queries; drawing never starts a process.
type workspaceMonitor struct {
	updates chan workspaceInfo
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func newWorkspaceMonitor(ctx context.Context, cwd string) *workspaceMonitor {
	ctx, cancel := context.WithCancel(ctx)
	m := &workspaceMonitor{updates: make(chan workspaceInfo, 1), cancel: cancel}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			v := inspectWorkspace(ctx, cwd)
			select {
			case m.updates <- v:
			case <-ctx.Done():
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return m
}
func (m *workspaceMonitor) close() { m.cancel(); m.wg.Wait() }

func inspectWorkspace(ctx context.Context, cwd string) workspaceInfo {
	v := workspaceInfo{cwd: cwd}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
		cmd.WaitDelay = 250 * time.Millisecond
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if err == syscall.ESRCH {
				return nil
			}
			return err
		}
		var out boundedGitOutput
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		if err != nil {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}
	var err error
	v.repo, err = run("rev-parse", "--show-toplevel")
	if err != nil {
		v.gitError = err.Error()
		return v
	}
	v.branch, err = run("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			v.gitError = ctx.Err().Error()
		} else {
			v.branch = "detached"
		}
	}
	return v
}

type boundedGitOutput struct{ bytes.Buffer }

func (b *boundedGitOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 16<<10 {
		_, _ = b.Buffer.Write(p[:min(n, (16<<10)-b.Len())])
	}
	return n, nil
}
