package rail

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/filelock"
)

func TestCanonicalWorkdir(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(root, "alias")
	dir := filepath.Join(root, "project")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	want, err := canonicalWorkdir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := canonicalWorkdir(alias)
	if err != nil || got != want || workdirKey(got) != workdirKey(want) {
		t.Fatalf("alias canonicalization: %s %v, want %s", got, err, want)
	}
	if _, err = canonicalWorkdir(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing workdir accepted")
	}
	if err = os.WriteFile(filepath.Join(root, "file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = canonicalWorkdir(filepath.Join(root, "file")); err == nil {
		t.Fatal("file workdir accepted")
	}
}

func TestPrivateRuntimeAndLifetimeLock(t *testing.T) {
	base, err := os.MkdirTemp("", "rail-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	t.Setenv("XDG_RUNTIME_DIR", base)
	root, err := runtimeRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, workdirKey("/test/project"))
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	if live, err := serverRunning(dir); err != nil || live {
		t.Fatalf("stale instance treated as live: %v %v", live, err)
	}
	lock, err := filelock.Acquire(context.Background(), filepath.Join(dir, "server.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if live, err := serverRunning(dir); err != nil || !live {
		t.Fatalf("live instance not detected: %v %v", live, err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if live, err := serverRunning(dir); err != nil || live {
		t.Fatalf("released lock still live: %v %v", live, err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(alias); err == nil {
		t.Fatal("symlink registry directory accepted")
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateDir(dir); err == nil {
		t.Fatal("public registry directory accepted")
	}
}

func TestRuntimeRejectsRelativeXDG(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "relative")
	if _, err := runtimeRoot(); err == nil {
		t.Fatal("relative runtime dir accepted")
	}
}

func TestClientEnvironmentAndSupervisorDispatch(t *testing.T) {
	t.Setenv("TMUX", "/tmp/host-server,123,0")
	t.Setenv("TMUX_PANE", "%1")
	t.Setenv("TTC_RAIL_ENV_FIXTURE", "preserved")
	for _, item := range clientEnvironment() {
		if strings.HasPrefix(item, "TMUX=") || strings.HasPrefix(item, "TMUX_PANE=") {
			t.Fatal("host tmux identity leaked", item)
		}
	}
	if !strings.Contains(strings.Join(clientEnvironment(), "\n"), "TTC_RAIL_ENV_FIXTURE=preserved") {
		t.Fatal("unrelated environment removed")
	}
	if !IsSupervisor([]string{supervisorCommand}) || IsSupervisor([]string{supervisorCommand, "extra"}) || IsSupervisor(nil) {
		t.Fatal("invalid supervisor dispatch")
	}
}

func TestListLiveSessionsAndIgnoreStale(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is optional outside rail runtime")
	}
	root, err := os.MkdirTemp("", "rail-list-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	workdir := "/fixture/project"
	dir := filepath.Join(root, workdirKey(workdir))
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(filepath.Join(dir, "run")); err != nil {
		t.Fatal(err)
	}
	lock, err := filelock.Acquire(context.Background(), filepath.Join(dir, "server.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	data, err := json.Marshal(instance{Version: 1, Workdir: workdir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instance.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("tmux", "-S", socketPath(dir), "-f", "/dev/null", "new-session", "-d", "-s", "rail", "sleep 60")
	cmd.Env = clientEnvironment()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tmux fixture: %v\n%s", err, output)
	}
	defer exec.Command("tmux", "-S", socketPath(dir), "kill-server").Run()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serviceDone := make(chan error, 1)
	go func() { serviceDone <- serveClients(ctx, controlPath(dir), socketPath(dir)) }()
	defer func() { cancel(); <-serviceDone }()
	for i := 0; i < 100; i++ {
		if _, err := sessions(ctx, controlPath(dir)); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var out bytes.Buffer
	if err := listInstances(context.Background(), root, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WORKDIR") || !strings.Contains(out.String(), workdir) || !strings.Contains(out.String(), "rail") {
		t.Fatal("live session missing", out.String())
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := listInstances(context.Background(), root, &out); err != nil || out.String() != "No live rail sessions.\n" {
		t.Fatalf("stale instance listed: %s %v", out.String(), err)
	}
}

func TestTmuxStartupQuotesAndLifetimeSettings(t *testing.T) {
	plan := sandboxPlan{workdir: "/project/with 'quotes'", tmuxConfig: "/home/user/.tmux.conf"}
	config := tmuxStartup(plan)
	for _, want := range []string{"exit-unattached off", "exit-empty on", "new-session -d -s rail", "#H"} {
		if !strings.Contains(config, want) {
			t.Fatalf("missing %s: %s", want, config)
		}
	}
	if tmuxQuote("a'b") != "'a'\\''b'" {
		t.Fatal("incorrect tmux quoting")
	}
}
