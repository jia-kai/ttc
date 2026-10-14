package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"ttc/internal/watchrun"
)

func watchFixture(t *testing.T, completed bool) (string, watchrun.Identity) {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	id := watchrun.Identity{Version: 1, ID: "test-run", PID: os.Getpid(), Start: fields[19], Boot: strings.TrimSpace(string(boot)), Command: []string{"test-workload"}, StartedAt: time.Now().Format(time.RFC3339Nano)}
	data, _ := json.Marshal(id)
	if err = os.WriteFile(filepath.Join(dir, "identity.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "stdout"), []byte("complete output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "stderr"), []byte("error stream\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if completed {
		data, _ = json.Marshal(watchrun.Completion{ID: id.ID, ExitCode: 9, FinishedAt: time.Now().Format(time.RFC3339Nano)})
		if err = os.WriteFile(filepath.Join(dir, "completion.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, id
}
func TestWatchCompletionReadAndNotification(t *testing.T) {
	dir, _ := watchFixture(t, true)
	notifications := make(chan Snapshot, 1)
	m := New(context.Background(), func(s Snapshot) { notifications <- s })
	defer m.Close()
	id, err := m.StartWatch("main/child", dir, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	view, err := m.Wait(ctx, "main", id, nil)
	if err != nil || view.Status != "completed" || view.Kind != "watch" || view.ExitCode == nil || *view.ExitCode != 9 || view.WakeOnExit {
		t.Fatalf("view %+v %v", view, err)
	}
	select {
	case n := <-notifications:
		if n.WakeOnExit || n.ExitCode == nil || *n.ExitCode != 9 {
			t.Fatalf("notification %+v", n)
		}
	case <-ctx.Done():
		t.Fatal("missing notification")
	}
	page, err := m.Read(ctx, "main", id, ReadOptions{Limit: 100})
	if err != nil || page["output"] != "complete output\n" {
		t.Fatalf("page %+v %v", page, err)
	}
	if _, err = m.View("other", id); err == nil {
		t.Fatal("watch leaked ownership")
	}
}
func TestWatchIncompleteOutputPreservesStatusButFails(t *testing.T) {
	dir, identity := watchFixture(t, true)
	data, err := json.Marshal(watchrun.Completion{ID: identity.ID, ExitCode: 0, Error: "incomplete output", FinishedAt: time.Now().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "completion.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), nil)
	defer m.Close()
	id, err := m.StartWatch("main", dir, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	view, err := m.Wait(ctx, "main", id, nil)
	if err != nil || view.Status != "failed" || view.ExitCode == nil || *view.ExitCode != 0 || !strings.Contains(view.Stderr, "incomplete output") {
		t.Fatalf("result: %+v %v", view, err)
	}
}

func TestWatchStopAndCloseOnlyObservation(t *testing.T) {
	for _, closeManager := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "close"}[closeManager], func(t *testing.T) {
			dir, _ := watchFixture(t, false)
			m := New(context.Background(), nil)
			defer m.Close()
			id, err := m.StartWatch("main", dir, true)
			if err != nil {
				t.Fatal(err)
			}
			if closeManager {
				m.Close()
			} else {
				view, err := m.Stop("main", id)
				if err != nil || view.Status != "cancelled" {
					t.Fatalf("stop %+v %v", view, err)
				}
			}
			// The identity points at this test process. Any attempted workload kill
			// would terminate the test itself; cancellation must leave logs untouched.
			b, err := os.ReadFile(filepath.Join(dir, "stdout"))
			if err != nil || string(b) != "complete output\n" {
				t.Fatalf("record changed: %q %v", b, err)
			}
		})
	}
}
