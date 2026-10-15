package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/jobs"
	"ttc/internal/watchrun"
)

func TestToolTmuxRecorderHelper(t *testing.T) {
	if os.Getenv("TTC_TOOL_TMUX_HELPER") != "1" {
		return
	}
	for i, a := range os.Args {
		if a == "watch-run" {
			code, err := watchrun.Run(os.Args[i+2], os.Args[i+4:], nil, os.Stdout, os.Stderr)
			if err != nil {
				os.Exit(99)
			}
			os.Exit(code)
		}
	}
	os.Exit(98)
}

func toolTmuxLauncher(t *testing.T) watchrun.TmuxLauncher {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	root := t.TempDir()
	socket := filepath.Join(root, "tmux.sock")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
	// An explicit integration run may exercise the built production executable;
	// ordinary unit tests remain self-contained through the recorder helper.
	if executable := os.Getenv("TTC_TEST_BINARY"); executable != "" {
		return watchrun.TmuxLauncher{Executable: executable, TmuxArgs: []string{"-S", socket, "-f", "/dev/null"}}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "recorder")
	if err = os.WriteFile(helper, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(exe, "'", "'\\''")+"' -test.run=^TestToolTmuxRecorderHelper$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTC_TOOL_TMUX_HELPER", "1")
	return watchrun.TmuxLauncher{Executable: helper, TmuxArgs: []string{"-S", socket, "-f", "/dev/null"}}
}

func TestTmuxWatchTool(t *testing.T) {
	launcher := toolTmuxLauncher(t)
	r, w, x, _ := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	addTmuxWatchRun(r, m, w, launcher)
	tool, _ := r.Get("tmux_watch_run")
	for _, args := range []string{`{}`, `{"command":"true"}`, `{"command":"true","record_dir":"r","unknown":1}`, `{"command":"true","record_dir":"r","label":"line\nbreak"}`} {
		if _, err := tool.DecodeCall(1, []byte(args)); err == nil {
			t.Fatalf("accepted %s", args)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := r.Invoke(ctx, x, "tmux_watch_run", []byte(`{"command":"sleep 0.2; printf durable; exit 9","record_dir":"record","label":"checks","wake_on_exit":false}`))
	var launch struct {
		Status      string        `json:"status"`
		JobID       string        `json:"job_id"`
		RecordDir   string        `json:"record_dir"`
		SessionName string        `json:"session_name"`
		PaneID      string        `json:"pane_id"`
		Watcher     jobs.Snapshot `json:"watcher"`
	}
	if err := json.Unmarshal(rec.Result, &launch); err != nil || launch.Status != "watching" || launch.JobID == "" || launch.PaneID == "" || launch.Watcher.WakeOnExit || launch.Watcher.Label != "checks" {
		t.Fatalf("launch %s %v", rec.Result, err)
	}
	if launch.RecordDir != filepath.Join(w.Root, "record") {
		t.Fatalf("record path %s", launch.RecordDir)
	}
	if _, err := m.Stop(x.Actor, launch.JobID); err != nil {
		t.Fatal(err)
	}
	// Watch stopping must not signal either external process. Reattach normally.
	id, err := m.StartWatch(x.Actor, launch.RecordDir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := m.Wait(ctx, x.Actor, id, nil)
	if err != nil || v.Stdout != "durable" || v.ExitCode == nil || *v.ExitCode != 9 {
		t.Fatalf("reattached %+v %v", v, err)
	}
	rec = r.Invoke(ctx, x, "tmux_watch_run", []byte(`{"command":"true","record_dir":"record"}`))
	if !strings.Contains(string(rec.Result), `"status":"launch_failed"`) {
		t.Fatalf("duplicate %s", rec.Result)
	}
}

func TestTmuxWatchAttachmentFailureDoesNotRelaunch(t *testing.T) {
	launcher := toolTmuxLauncher(t)
	r, w, x, _ := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	m.Close() // The detached launch can succeed even when observation is closed.
	addTmuxWatchRun(r, m, w, launcher)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := r.Invoke(ctx, x, "tmux_watch_run", []byte(`{"command":"printf once","record_dir":"record"}`))
	if !strings.Contains(string(rec.Result), `"status":"launched_unobserved"`) {
		t.Fatalf("attachment failure %s", rec.Result)
	}
	id, err := watchrun.ReadIdentity(filepath.Join(w.Root, "record"))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	_, err = watchrun.Observe(ctx, filepath.Join(w.Root, "record"), id, &out, &strings.Builder{})
	if err != nil || out.String() != "once" {
		t.Fatalf("external run %q %v", out.String(), err)
	}
}
