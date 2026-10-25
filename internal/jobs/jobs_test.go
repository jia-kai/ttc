package jobs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/capture"
)

func TestSeparateStreamsTailEOFGrepAndTaskCancellation(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	id, err := m.Start("main", "i=1; while [ $i -le 20 ]; do printf 'out-%s\\n' $i; printf 'err-%s\\n' $i >&2; i=$((i+1)); done", t.TempDir(), time.Second, true, false, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := m.Wait(context.Background(), "main", id, nil)
	if err != nil || !strings.HasPrefix(v.Stdout, "out-1\n") || !strings.HasSuffix(v.Stdout, "out-20\n") || !strings.HasPrefix(v.Stderr, "err-1\n") || v.Truncated || len(v.Stdout)+len(v.Stderr) > 8192 {
		t.Fatal(v, err)
	}
	page, err := m.Read(context.Background(), "main", id, ReadOptions{Stream: "stderr", Limit: 1024, Grep: "ERR-(18|20)", IgnoreCase: true})
	if err != nil || page["output"] != "err-18\nerr-20\n" || page["next_cursor"] != nil {
		t.Fatal(page, err)
	}
	if page["kind"] != "grep" || page["grep_origin"].(GrepOrigin).MatchCount != 2 {
		t.Fatal(page)
	}
	for _, options := range []ReadOptions{{Stream: "combined", Limit: 16}, {Grep: "[", Limit: 16}, {Cursor: "eof:1:bytes", Limit: 16}} {
		if _, err := m.Read(context.Background(), "main", id, options); err == nil {
			t.Fatal("accepted invalid options", options)
		}
	}
	ready := make(chan string, 1)
	childID, err := m.StartTask("main/child", TaskOptions{Kind: "subagent", Label: "test", Background: true}, func(ctx context.Context, stdout, stderr io.Writer) (TaskResult, error) {
		id := <-ready
		if _, err := m.Stop("main/child", id); err == nil {
			t.Error("child self-stop allowed")
		}
		stdout.Write([]byte{0xff, '\n', 'x', '\n'})
		<-ctx.Done()
		return TaskResult{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	ready <- childID
	deadline := time.Now().Add(time.Second)
	for {
		page, err = m.Read(context.Background(), "main", childID, ReadOptions{Limit: 16, Grep: "x", Literal: true})
		if err != nil {
			t.Fatal(err)
		}
		if page["output"] == "x\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child self-stop deadlocked")
		}
		time.Sleep(time.Millisecond)
	}
	if page["grep_origin"].(GrepOrigin).MatchCount != 1 {
		t.Fatal(page)
	}
	v, err = m.Stop("main", childID)
	if err != nil || v.Status != "cancelled" {
		t.Fatal(v, err)
	}
}

func TestShellSupervisionFailureExplainsRecovery(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	id, err := m.Start("main", "printf 'leader finished\\n'; sleep 5 &", t.TempDir(), 3*time.Second, true, false, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	view, err := m.Wait(ctx, "main", id, nil)
	if err != nil || view.Status != "failed" || view.ExitCode == nil || *view.ExitCode != 0 {
		t.Fatal("expected output-pipe supervision failure after successful leader exit", view, err)
	}
	if !strings.Contains(view.Stdout, "leader finished") || !strings.Contains(view.Stderr, "Shell supervision failed") || !strings.Contains(view.Stderr, "output pipes") || !strings.Contains(view.Stderr, "background=true") || !strings.Contains(view.Stderr, "tmux") {
		t.Fatal("supervision failure lost output or recovery guidance", view)
	}
}

func TestForegroundExitStatusBackgroundNotificationAndStaleHandles(t *testing.T) {
	notify := make(chan Snapshot, 2)
	m := New(context.Background(), func(v Snapshot) { notify <- v })
	root := t.TempDir()
	id, e := m.Start("main", "printf hello; exit 7", root, time.Second, true, false, capture.OutputOptions{}, "")
	if e != nil {
		t.Fatal(e)
	}
	v, e := m.Wait(context.Background(), "main", id, nil)
	if e != nil || v.Status != "completed" || v.ExitCode == nil || *v.ExitCode != 7 || v.Stdout != "hello" {
		t.Fatal(v, e)
	}
	select {
	case <-notify:
		t.Fatal("duplicate foreground notification")
	default:
	}
	id, e = m.Start("main", "printf background", root, time.Second, true, true, capture.OutputOptions{}, "")
	if e != nil {
		t.Fatal(e)
	}
	select {
	case v := <-notify:
		if v.ID != id {
			t.Fatal(v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no background notification")
	}
	page, e := m.Read(context.Background(), "main", id, ReadOptions{Limit: 3})
	if e != nil || page["output"] != "bac" || page["next_cursor"] == nil {
		t.Fatal(page, e)
	}
	if _, e = m.View("other", id); e == nil {
		t.Fatal("cross-actor access")
	}
	m.Close()
	if _, e = m.View("main", id); e == nil {
		t.Fatal("revived stale handle")
	}
}
func TestCancellationStopsProcessGroupAndBoundedLogs(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	root := t.TempDir()
	marker := filepath.Join(root, "leak")
	id, e := m.Start("main", "(sleep 1; touch '"+marker+"') & wait", root, 0, true, false, capture.OutputOptions{}, "")
	if e != nil {
		t.Fatal(e)
	}
	v, e := m.Stop("main", id)
	if e != nil || v.Status != "cancelled" {
		t.Fatal(v, e)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, e = os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("descendant escaped")
	}
	id, e = m.Start("main", "head -c 400000 /dev/zero | tr '\\0' x", root, time.Second, true, false, capture.OutputOptions{}, "")
	if e != nil {
		t.Fatal(e)
	}
	v, e = m.Wait(context.Background(), "main", id, nil)
	if e != nil || !v.Truncated || len(v.Stdout) > 8192 || !strings.HasPrefix(v.Stdout, "xxx") {
		t.Fatal(len(v.Stdout), e)
	}
}

func TestWaitPublishesBoundedStreamingSnapshots(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	root := t.TempDir()
	id, err := m.Start("main", "printf 'first\\n'; while [ ! -e release ]; do sleep 0.01; done; printf 'last\\n'", root, 5*time.Second, true, false, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	updates := make(chan Snapshot, 8)
	done := make(chan error, 1)
	go func() {
		_, err := m.Wait(context.Background(), "main", id, func(v Snapshot) {
			select {
			case updates <- v:
			default:
			}
		})
		done <- err
	}()
	seen := 0
	deadline := time.After(3 * time.Second)
	for seen < 2 {
		select {
		case v := <-updates:
			if v.Status != "running" || len(v.Stdout)+len(v.Stderr) > 1024 {
				t.Fatal(v)
			}
			if strings.Contains(v.Stdout, "first") {
				seen++
			}
		case <-deadline:
			t.Fatal("missing repeated output updates")
		}
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait worker did not stop")
	}
	view, err := m.View("main", id)
	if err != nil || view.Status != "completed" || !strings.Contains(view.Stdout, "last") {
		t.Fatal(view, err)
	}
}
