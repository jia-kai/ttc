package watchrun

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTmuxRecorderHelper supplies the production recorder entrypoint from the
// test executable, without building or depending on an installed TTC binary.
func TestTmuxRecorderHelper(t *testing.T) {
	if os.Getenv("TTC_TMUX_RECORDER_HELPER") != "1" {
		return
	}
	for i, a := range os.Args {
		if a == "watch-run" {
			code, err := Run(os.Args[i+2], os.Args[i+4:], nil, os.Stdout, os.Stderr)
			if err != nil {
				os.Exit(99)
			}
			os.Exit(code)
		}
	}
	os.Exit(98)
}

func tmuxFixture(t *testing.T) TmuxLauncher {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	socket := filepath.Join(root, "tmux.sock")
	args := []string{"-S", socket, "-f", "/dev/null"}
	run := func(a ...string) {
		t.Helper()
		out, err := exec.Command("tmux", append(append([]string{}, args...), a...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v %s", a, err, out)
		}
	}
	run("new-session", "-d", "-s", "fixture", "/bin/sleep 60")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
	// Exercise inherited configuration and indexes; launched sessions must still
	// have one pane and disappear after even the fastest workload exits.
	run("set-option", "-g", "base-index", "7")
	run("set-option", "-g", "pane-base-index", "4")
	run("set-option", "-g", "remain-on-exit", "on")
	run("set-environment", "-g", "TTC_TMUX_STALE", "server-only")
	t.Setenv("TTC_TMUX_RECORDER_HELPER", "1")
	t.Setenv("TTC_TMUX_CURRENT", "host-current")
	t.Setenv("TMUX_PANE", "%caller-pane")
	t.Setenv("TTC_TMUX_STALE", "")
	// Unsetting rather than replacing proves env -i clears stale server values.
	if err := os.Unsetenv("TTC_TMUX_STALE"); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "recorder")
	if err = os.WriteFile(helper, []byte("#!/bin/sh\nexec "+quoteShell(exe)+" -test.run=^TestTmuxRecorderHelper$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return TmuxLauncher{Executable: helper, TmuxArgs: args}
}

func TestTmuxLaunchDurable(t *testing.T) {
	launcher := tmuxFixture(t)
	root := t.TempDir()
	dir := filepath.Join(root, "records")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := `printf '%s\n' "$TTC_TMUX_CURRENT" "${TTC_TMUX_STALE-unset}" "$PWD" "$TMUX_PANE"; printf 'stderr\n' >&2; exit 7`
	v, err := launcher.Launch(ctx, command, "short label", dir, root)
	if err != nil || !v.Started || v.SessionID == "" || v.PaneID == "" {
		t.Fatalf("launch %+v %v", v, err)
	}
	if err = WaitReady(ctx, dir); err != nil {
		t.Fatal(err)
	}
	id, err := ReadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr strings.Builder
	c, err := Observe(ctx, dir, id, &out, &stderr)
	if err != nil || c.ExitCode != 7 || out.String() != "host-current\nunset\n"+root+"\n"+v.PaneID+"\n" || stderr.String() != "stderr\n" {
		t.Fatalf("completion %+v %v %q %q", c, err, out.String(), stderr.String())
	}
	// Completion publication precedes recorder exit by a small scheduling gap.
	for {
		err = exec.Command("tmux", append(append([]string{}, launcher.TmuxArgs...), "has-session", "-t", v.SessionName)...).Run()
		if err != nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("session remained after exit")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestTmuxLaunchCancellationAndValidation(t *testing.T) {
	launcher := tmuxFixture(t)
	root := t.TempDir()
	dir := filepath.Join(root, "records")
	ctx, cancel := context.WithCancel(context.Background())
	v, err := launcher.Launch(ctx, "sleep 0.1; printf survived", "", dir, root)
	if err != nil {
		t.Fatal(err)
	}
	cancel() // No connection from this context to the detached recorder.
	observe, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err = WaitReady(observe, dir); err != nil {
		t.Fatal(err)
	}
	id, err := ReadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	c, err := Observe(observe, dir, id, &out, io.Discard)
	if err != nil || c.ExitCode != 0 || out.String() != "survived" {
		t.Fatalf("survival %+v %v %q", c, err, out.String())
	}
	if !v.Started {
		t.Fatal("not launched")
	}
	duplicate, err := launcher.Launch(observe, "touch duplicate", "", dir, root)
	if err == nil || duplicate.Started {
		t.Fatalf("duplicate %+v %v", duplicate, err)
	}
	canceled, err := launcher.Launch(ctx, "true", "", filepath.Join(root, "cancel"), root)
	if !errors.Is(err, context.Canceled) || canceled.Started {
		t.Fatalf("canceled %+v %v", canceled, err)
	}
}

func TestTmuxClientCancellationIsAmbiguousAndDoesNotKill(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "client-finished")
	client := filepath.Join(root, "tmux")
	// A delayed client models a server request whose side effects may already
	// have committed when the caller's deadline expires.
	if err := os.WriteFile(client, []byte("#!/bin/sh\nsleep 0.15\nprintf done > "+quoteShell(marker)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	v, err := (TmuxLauncher{}).Launch(ctx, "true", "", filepath.Join(root, "record"), root)
	if !errors.Is(err, context.DeadlineExceeded) || !v.Started || v.SessionName == "" {
		t.Fatalf("ambiguous client %+v %v", v, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request cancellation killed the external client")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTmuxFaultClientHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "tmux-fault-helper" {
			continue
		}
		args := append([]string{}, os.Args[i+2:]...)
		for j, arg := range args {
			if arg == "off" {
				args[j] = "invalid-option-value"
			}
		}
		cmd := exec.Command(os.Args[i+1], args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestTmuxPartialLaunchIdentityIgnoresDiagnostics(t *testing.T) {
	root := t.TempDir()
	client := filepath.Join(root, "tmux")
	// Force the problematic cross-stream ordering instead of depending on
	// scheduling between tmux's buffered stdout and immediate error output.
	script := "#!/bin/sh\nprintf 'injected option error\\n' >&2\nprintf '$42 %%43\\n'\nexit 1\n"
	if err := os.WriteFile(client, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	v, err := (TmuxLauncher{}).Launch(context.Background(), "true", "", filepath.Join(root, "record"), root)
	if err == nil || !v.Started || v.SessionID != "$42" || v.PaneID != "%43" {
		t.Fatalf("partial identity %+v %v", v, err)
	}
}

func TestTmuxPartialLaunchStagingLease(t *testing.T) {
	launcher := tmuxFixture(t)
	launcher.stagingTimeout = 100 * time.Millisecond
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Fail set-option at execution time, after new-session has created a pane.
	// Parsing an unknown command would instead fail before any pane exists.
	client := filepath.Join(root, "tmux")
	script := "#!/bin/sh\nexec " + quoteShell(exe) + " -test.run=^TestTmuxFaultClientHelper$ -- tmux-fault-helper " + quoteShell(realTmux) + " \"$@\"\n"
	if err = os.WriteFile(client, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := filepath.Join(root, "record")
	v, err := launcher.Launch(ctx, "printf should-not-launch", "", dir, root)
	if err == nil || !v.Started || v.PaneID == "" {
		t.Fatalf("partial launch %+v %v", v, err)
	}
	for {
		err := exec.Command(realTmux, append(append([]string{}, launcher.TmuxArgs...), "has-session", "-t", v.SessionName)...).Run()
		if err != nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("staging lease left a retained pane after partial launch failure")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected workload records: %v", err)
	}
}

func TestTmuxExpiredStagingCannotKillRespawnedWorkload(t *testing.T) {
	launcher := tmuxFixture(t)
	launcher.stagingTimeout = 20 * time.Millisecond
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ready, survived := filepath.Join(root, "cleanup-started"), filepath.Join(root, "survived")
	finished := filepath.Join(root, "cleanup-finished")
	client := filepath.Join(root, "cleanup-client")
	// Hold an already-expired cleanup request until the staging process has
	// been replaced, so its captured PID guard must reject the new workload.
	script := "#!/bin/sh\ntrap '' HUP\nprintf ready > " + quoteShell(ready) + "; sleep 0.2; " + quoteShell(realTmux) + " \"$@\"; printf finished > " + quoteShell(finished) + "\n"
	if err := os.WriteFile(client, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(realTmux, append(append([]string{}, launcher.TmuxArgs...), args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	pane := run("new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "lease-race", launcher.stagingCommand(client))
	waitFile := func(path string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(path); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("missing %s", path)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFile(ready)
	run("set-option", "-w", "-t", pane, "remain-on-exit", "off")
	run("respawn-pane", "-k", "-t", pane, "sleep 0.4; touch "+quoteShell(survived))
	waitFile(finished)
	waitFile(survived)
}
