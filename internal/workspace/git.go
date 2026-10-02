package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// GitInfo is bounded optional Git metadata for a workspace. An empty Repo means
// Git inspection did not establish a repository; Error explains unavailable metadata.
type GitInfo struct{ Cwd, Repo, Branch, Error string }

// Detail returns terminal-readable workspace metadata.
func (v GitInfo) Detail() string {
	text := "Working directory:\n" + v.Cwd
	if v.Repo != "" {
		text += "\n\nGit repository:\n" + v.Repo
		if v.Branch != "" {
			text += "\n\nBranch:\n" + v.Branch
		}
	} else {
		text += "\n\nGit: no repository available"
	}
	if v.Error != "" {
		text += "\n" + v.Error
	}
	return text
}

// InspectGit queries optional Git metadata with a two-second deadline.
// It never runs in the draw loop and kills the process group on cancellation.
func InspectGit(ctx context.Context, cwd string) GitInfo {
	v := GitInfo{Cwd: cwd}
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
	v.Repo, err = run("rev-parse", "--show-toplevel")
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Branch, err = run("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			v.Error = ctx.Err().Error()
		} else {
			v.Branch = "detached"
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
