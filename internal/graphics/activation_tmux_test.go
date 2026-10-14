package graphics

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This uses a private server and a PTY-backed normal client. Control observers
// count as attached clients; verify both ordinary stop and last-real-client
// detach rather than treating no-output/ignore-size as attachment neutrality.
func TestTmuxActivationRealClientCleanup(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script PTY helper unavailable")
	}
	socket := filepath.Join(t.TempDir(), "tmux.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tmux := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, "tmux", append([]string{"-S", socket, "-f", "/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	t.Setenv("TMUX", "")
	pane := tmux("new-session", "-d", "-s", "activation", "-P", "-F", "#{pane_id}", "sleep 30")
	defer func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() }()
	hookLog := filepath.Join(filepath.Dir(socket), "hooks.log")
	// These hooks belong solely to this private fixture; production recovery
	// never changes user hooks. Observe the client's real attach/detach effects.
	tmux("set-hook", "-g", "client-attached", fmt.Sprintf("run-shell %q", fmt.Sprintf("echo attached >> %q", hookLog)))
	tmux("set-hook", "-g", "client-detached", fmt.Sprintf("run-shell %q", fmt.Sprintf("echo detached >> %q", hookLog)))
	// The pipe stays open so script does not send EOF to the normal client.
	client := exec.CommandContext(ctx, "script", "-q", "-c", fmt.Sprintf("tmux -S %q attach-session -t activation", socket), "/dev/null")
	client.Env = append(os.Environ(), "TERM=xterm-256color")
	stdin, err := client.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	client.Stdout, client.Stderr = io.Discard, io.Discard
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Wait() }()
	defer func() { _ = client.Process.Kill() }()
	waitCount := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			got := tmux("display-message", "-p", "-t", pane, "#{session_attached}")
			if got == want {
				t.Logf("session_attached=%s", got)
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("session_attached=%s, want %s", got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitCount("1")
	realClient := tmux("list-clients", "-F", "#{client_name}")
	t.Setenv("TMUX", socket+",0,0")
	t.Setenv("TMUX_PANE", pane)
	_, stop, err := WatchTmuxActivation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	waitCount("2")
	stop()
	waitCount("1")
	_, stopSecond, err := WatchTmuxActivation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stopSecond()
	waitCount("2")
	tmux("detach-client", "-t", realClient)
	waitCount("1")
	if got := tmux("list-clients", "-F", "#{client_control_mode}"); got != "1" {
		t.Fatalf("normal client remained attached: %q", got)
	}
	stopSecond()
	waitCount("0")
	deadline := time.Now().Add(time.Second)
	for {
		data, err := os.ReadFile(hookLog)
		if err == nil && strings.Count(string(data), "attached\n") == 3 && strings.Count(string(data), "detached\n") == 3 {
			t.Logf("private client hooks: %s", strings.TrimSpace(string(data)))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("attach/detach hooks did not observe all clients: %q: %v", data, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-clientDone:
		if err != nil {
			t.Fatal("normal client failed after detach", err)
		}
	case <-time.After(time.Second):
		t.Fatal("normal client did not exit after detach")
	}
}
