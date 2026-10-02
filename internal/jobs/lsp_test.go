package jobs

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"scicode/internal/lsp"
)

func serverCommand(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("stdio fixture requires python3")
	}
	fixture, err := filepath.Abs("../lsp/testdata/server.py")
	if err != nil {
		t.Fatal(err)
	}
	quote := func(text string) string { return "'" + strings.ReplaceAll(text, "'", "'\"'\"'") + "'" }
	return "exec " + quote(python) + " " + quote(fixture)
}

func TestLSPJobsOwnProtocolAndCancelQueries(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.py")
	if err := os.WriteFile(path, []byte("a😀éz\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), nil)
	defer m.Close()
	id, err := m.StartLSP("main/child", serverCommand(t), root, 0, true, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	q := lsp.Query{Operation: "hover", Path: path, Line: 1, Column: 3, Limit: 100}
	result, err := m.QueryLSP(ctx, "main", id, q)
	if err != nil || !strings.Contains(result["markdown"].(string), "a😀éz") {
		t.Fatal(result, err)
	}
	if _, err := m.QueryLSP(ctx, "main/other", id, q); err == nil {
		t.Fatal("unrelated child accessed LSP")
	}
	if _, err := m.Read(ctx, "main", id, ReadOptions{Stream: "stdout", Limit: 1024}); err == nil {
		t.Fatal("protocol stdout exposed")
	}
	page, err := m.Read(ctx, "main", id, ReadOptions{Limit: 4096})
	if err != nil || page["stream"] != "stderr" || strings.Contains(page["output"].(string), "Content-Length:") {
		t.Fatal(page, err)
	}
	v, err := m.Stop("main", id)
	if err != nil || v.Kind != "lsp" || v.Status != "cancelled" {
		t.Fatal(v, err)
	}
	_, err = m.QueryLSP(ctx, "main", id, q)
	var failure *lsp.Error
	if !errors.As(err, &failure) || failure.Code != "job_not_running" {
		t.Fatal(err)
	}
	// Stop must interrupt a request stalled waiting for the server.
	id, err = m.StartLSP("main", "TTC_LSP_MODE=slow "+serverCommand(t), root, 0, true, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := m.QueryLSP(ctx, "main", id, q); done <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		page, err = m.Read(ctx, "main", id, ReadOptions{Limit: 4096})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(page["output"].(string), "slow request received") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not receive request", page)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := m.Stop("main", id); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stopped query succeeded")
		}
	case <-ctx.Done():
		t.Fatal("query survived stopped server")
	}
}

func TestStatePublicationPrecedesWorkerAndWait(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	var mu sync.Mutex
	states := map[string][]string{}
	m.OnState = func(view Snapshot) {
		// A callback must be able to inspect manager metadata without a lock cycle.
		_ = m.Metadata("main")
		mu.Lock()
		defer mu.Unlock()
		states[view.ID] = append(states[view.ID], view.Status)
	}
	check := func(id string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		got := states[id]
		if len(got) != 2 || got[0] != "running" || got[1] == "running" {
			t.Fatal(id, got)
		}
	}
	for _, background := range []bool{false, true} {
		id, err := m.Start("main", "true", t.TempDir(), time.Second, true, background, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Wait(context.Background(), "main", id, nil); err != nil {
			t.Fatal(err)
		}
		check(id)
	}
	id, err := m.StartTask("main", "subagent", "test", true, false, func(ctx context.Context, out, stderr io.Writer) error {
		mu.Lock()
		defer mu.Unlock()
		if len(states) != 3 {
			t.Errorf("worker ran before launch publication: %v", states)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	check(id)
	id, err = m.StartLSP("main", serverCommand(t), t.TempDir(), 0, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stop("main", id); err != nil {
		t.Fatal(err)
	}
	check(id)
}
