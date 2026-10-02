package jobs

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPromotedWaitJoinsTerminalPublication(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	m := New(context.Background(), nil)
	m.OnState = func(v Snapshot) {
		if v.Status != "running" {
			close(entered)
			<-release
		}
	}
	defer m.Close()
	defer unblock()
	root := t.TempDir()
	id, err := m.Start("main", "while [ ! -e release ]; do sleep 0.01; done", root, 0, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Promote("main", id); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal publication did not start")
	}
	done := make(chan error, 1)
	go func() {
		_, err := m.Wait(context.Background(), "main", id, nil)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("promoted waiter crossed unpublished terminal state: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("promoted waiter did not finish after terminal publication")
	}
}
