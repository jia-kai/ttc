package graphics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// probeTmux reads tmux's detected client identity and effective pane setting.
// Kitty's icat also avoids response-based detection through tmux: graphics
// passthrough does not guarantee a usable return path for acknowledgments.
func probeTmux(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	args := []string{"display-message", "-p"}
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		args = append(args, "-t", pane)
	}
	args = append(args, "#{client_termtype}\n#{allow-passthrough}\n#{client_termfeatures}")
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.WaitDelay = 250 * time.Millisecond
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	var output probeOutput
	stderr := probeOutput{limit: 1024}
	cmd.Stdout = &output
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("tmux graphics metadata: %w", ctx.Err())
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return fmt.Errorf("cannot read tmux graphics metadata: %w: %q", err, detail)
		}
		return fmt.Errorf("cannot read tmux graphics metadata: %w", err)
	}
	if output.overflow {
		return errors.New("tmux graphics metadata exceeds 4 KiB")
	}
	return tmuxGraphics(output.String())
}

func tmuxGraphics(metadata string) error {
	fields := strings.Split(strings.TrimSuffix(metadata, "\n"), "\n")
	if len(fields) != 3 {
		return errors.New("invalid tmux graphics metadata")
	}
	if fields[1] != "on" && fields[1] != "all" {
		return errors.New("tmux graphics passthrough is disabled; enable allow-passthrough for this pane")
	}
	if !strings.HasPrefix(fields[0], "kitty(") || !strings.HasSuffix(fields[0], ")") {
		return errors.New("tmux did not identify its client as Kitty; check client_termtype")
	}
	for _, feature := range strings.Split(fields[2], ",") {
		if feature == "RGB" {
			return nil
		}
	}
	return errors.New("tmux client lacks RGB support; configure terminal-features for its terminal name")
}

type probeOutput struct {
	buffer   bytes.Buffer
	overflow bool
	limit    int // Retained bytes; zero uses the 4 KiB metadata limit.
}

func (b *probeOutput) Write(p []byte) (int, error) {
	n := len(p)
	limit := b.limit
	if limit == 0 {
		limit = 4096
	}
	keep := min(n, limit-b.buffer.Len())
	_, _ = b.buffer.Write(p[:keep])
	b.overflow = b.overflow || keep != n
	return n, nil
}

func (b *probeOutput) String() string { return b.buffer.String() }
