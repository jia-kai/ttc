package graphics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// WatchTmuxActivation reports window-return notifications without relying on
// terminal focus reporting or changing the user's tmux options. The control
// client suppresses pane output and leaves terminal sizing and the session
// environment unchanged, but counts as attached and runs the user's client
// attach/detach hooks.
// stop cancels and joins the client; callers must call it before terminal exit.
func WatchTmuxActivation(ctx context.Context) (<-chan struct{}, func(), error) {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return nil, nil, fmt.Errorf("tmux activation: TMUX_PANE is missing")
	}
	lookupCtx, cancelLookup := context.WithTimeout(ctx, 2*time.Second)
	defer cancelLookup()
	lookup := exec.CommandContext(lookupCtx, "tmux", "display-message", "-p", "-t", pane, "#{session_id} #{window_id}")
	lookup.WaitDelay = 250 * time.Millisecond
	var metadata probeOutput
	lookup.Stdout = &metadata
	if err := lookup.Run(); err != nil {
		return nil, nil, fmt.Errorf("tmux activation target: %w", err)
	}
	fields := strings.Fields(metadata.String())
	if metadata.overflow || len(fields) != 2 || !strings.HasPrefix(fields[0], "$") || !strings.HasPrefix(fields[1], "@") {
		return nil, nil, fmt.Errorf("invalid tmux activation target: %q", metadata.String())
	}
	ctx, cancel := context.WithCancel(ctx)
	// -E prevents update-environment from applying TTC's stripped agent socket
	// (or other inherited values) to the user's shared tmux session.
	cmd := exec.CommandContext(ctx, "tmux", "-C", "attach-session", "-E", "-f", "no-output,ignore-size", "-t", fields[0])
	cmd.WaitDelay = 250 * time.Millisecond
	// Control mode needs a live input pipe, though this observer sends no commands.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		stdin.Close()
		return nil, nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		stdin.Close()
		return nil, nil, fmt.Errorf("start tmux activation observer: %w", err)
	}
	events := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(events)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if tmuxActivation(scanner.Text(), fields[0], fields[1]) {
				select {
				case events <- struct{}{}:
				default:
				}
			}
		}
		// A malformed/oversized control notification must not leave a live
		// attached client after the scanner stops consuming its output.
		cancel()
		_ = cmd.Wait()
	}()
	stop := func() { cancel(); _ = stdin.Close(); <-done }
	return events, stop, nil
}

func tmuxActivation(line, session, window string) bool {
	fields := strings.Fields(line)
	if len(fields) == 3 && fields[0] == "%session-window-changed" {
		return fields[1] == session && fields[2] == window
	}
	// A real terminal can detach/re-attach or return from a different session
	// without changing this session's current window.
	return len(fields) >= 3 && fields[0] == "%client-session-changed" && fields[2] == session
}
