package watchrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// TmuxLauncher launches externally owned recorder processes. Zero values use
// tmux on PATH and the current executable. Overrides are intended for tests.
type TmuxLauncher struct {
	Executable string
	// Prefix arguments select an isolated tmux socket in tests.
	TmuxArgs []string
	// stagingTimeout overrides the 60-second staging lease in package tests.
	stagingTimeout time.Duration
}

// TmuxLaunch preserves recovery identity even when a launch's outcome is unknown.
// Started means the tmux client started, not that the workload necessarily did.
type TmuxLaunch struct {
	SessionName string `json:"session_name"`
	SessionID   string `json:"session_id,omitempty"`
	PaneID      string `json:"pane_id,omitempty"`
	RecordDir   string `json:"record_dir"`
	Started     bool   `json:"-"`
}

func quoteShell(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// Launch creates exactly one detached session/pane and starts a strict shell
// through the durable recorder. No process is owned by the request context:
// cancellation after client start returns an ambiguous launch without killing
// the client, server, recorder or workload. Callers must never retry that launch.
// The environment is replaced explicitly, avoiding stale tmux-server variables.
func (l TmuxLauncher) Launch(ctx context.Context, command, label, recordDir, workdir string) (TmuxLaunch, error) {
	v := TmuxLaunch{RecordDir: recordDir}
	if err := ctx.Err(); err != nil {
		return v, err
	}
	if strings.TrimSpace(command) == "" {
		return v, syscall.EINVAL
	}
	var err error
	v.RecordDir, err = filepath.Abs(recordDir)
	if err != nil {
		return v, err
	}
	if _, err = os.Lstat(v.RecordDir); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = os.ErrExist
		}
		return v, err
	}
	if info, e := os.Stat(filepath.Dir(v.RecordDir)); e != nil {
		return v, e
	} else if !info.IsDir() {
		return v, syscall.ENOTDIR
	}
	workdir, err = filepath.Abs(workdir)
	if err != nil {
		return v, err
	}
	if info, e := os.Stat(workdir); e != nil {
		return v, e
	} else if !info.IsDir() {
		return v, syscall.ENOTDIR
	}
	executable := l.Executable
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return v, err
		}
	}
	if _, err = os.Stat(executable); err != nil {
		return v, err
	}
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return v, err
	}
	env, err := exec.LookPath("env")
	if err != nil {
		return v, err
	}
	var token [12]byte
	if _, err = rand.Read(token[:]); err != nil {
		return v, err
	}
	v.SessionName = "ttc-run-" + hex.EncodeToString(token[:])
	// exec bypasses the server's default shell after its initial command parse.
	// env -i removes stale entries, including variables unset by the TTC host.
	argv := []string{env, "-i"}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TMUX=") && !strings.HasPrefix(entry, "TMUX_PANE=") {
			argv = append(argv, entry)
		}
	}
	words := make([]string, 0, len(argv)+12)
	for _, a := range argv {
		words = append(words, quoteShell(a))
	}
	// Keep the new pane's tmux coordinates, not the caller's pane identity.
	words = append(words, `"TMUX=$TMUX"`, `"TMUX_PANE=$TMUX_PANE"`)
	for _, a := range []string{executable, "watch-run", "--record-dir", v.RecordDir, "--", "/bin/sh", "-eu", "-c", command} {
		words = append(words, quoteShell(a))
	}
	if label == "" {
		label = "workload"
	}
	args := append([]string{}, l.TmuxArgs...)
	args = append(args, "new-session", "-d", "-P", "-F", "#{session_id} #{pane_id}", "-s", v.SessionName, "-n", label, "-c", workdir, l.stagingCommand(tmux), ";", "set-option", "-w", "-t", v.SessionName+":", "remain-on-exit", "off", ";", "respawn-pane", "-k", "-t", v.SessionName+":", "-c", workdir, "exec "+strings.Join(words, " "))
	cmd := exec.Command(tmux, args...)
	// Capture privately: a canceled caller must not race an ongoing client write.
	type result struct {
		output []byte
		err    error
	}
	done := make(chan result, 1)
	if err := ctx.Err(); err != nil {
		return v, err
	}
	// Start separately to distinguish definite pre-launch failure.
	var output strings.Builder
	cmd.Stdout = &output
	// Diagnostics may precede new-session's buffered identity output even when
	// a later queue command fails. Never mix them into the identity protocol.
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		return v, err
	}
	v.Started = true
	go func() { err := cmd.Wait(); done <- result{[]byte(output.String()), err} }()
	select {
	case <-ctx.Done():
		return v, ctx.Err()
	case r := <-done:
		fields := strings.Fields(string(r.output))
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "$") && strings.HasPrefix(fields[1], "%") {
			v.SessionID = fields[0]
			v.PaneID = fields[1]
		}
		// Client diagnostic output can include the wrapper invocation/environment;
		// never expose it in model-facing errors.
		return v, r.err
	}
}

// stagingCommand bounds partial-launch residue, including dead panes retained
// by inherited remain-on-exit. The initial exec makes $$ equal pane_pid. Cleanup
// is a format-only tmux conditional: its check and inserted kill command execute
// in the server queue without yielding to an asynchronous shell. A respawned
// workload has a different pane_pid and must never be cleaned up by this lease.
func (l TmuxLauncher) stagingCommand(tmux string) string {
	timeout := l.stagingTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	words := []string{quoteShell(tmux)}
	for _, arg := range l.TmuxArgs {
		words = append(words, quoteShell(arg))
	}
	script := "/bin/sleep " + strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64) + "; exec " + strings.Join(words, " ") +
		` if-shell -F -t "$TMUX_PANE" "#{==:#{pane_pid},$$}" "kill-pane -t $TMUX_PANE"`
	return "exec /bin/sh -c " + quoteShell(script)
}

// WaitReady waits only for immutable recorder metadata, not workload completion.
// Cancellation stops readiness observation without affecting the launched work.
func WaitReady(ctx context.Context, dir string) error {
	timer := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
	for {
		if _, err := ReadIdentity(dir); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
