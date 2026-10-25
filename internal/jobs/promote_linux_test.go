package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ttc/internal/capture"
)

func TestPromoteReleasesForegroundAndKeepsCommandAlive(t *testing.T) {
	notices := make(chan Snapshot, 2)
	m := New(context.Background(), func(v Snapshot) { notices <- v })
	defer m.Close()
	root := t.TempDir()
	id, err := m.Start("main", "printf ready; while [ ! -e release ]; do sleep 0.01; done; printf done", root, 0, true, false, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan Snapshot, 1)
	go func() {
		v, e := m.Wait(ctx, "main", id, nil)
		if e != nil {
			t.Error(e)
		}
		result <- v
	}()
	if list := m.Foreground("main"); len(list) != 1 || list[0].ID != id {
		t.Fatal(list)
	}
	if _, err = m.Promote("main/child", id); err == nil {
		t.Fatal("foreign promotion allowed")
	}
	if _, err = m.Promote("main", id); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-result:
		if v.Status != "running" {
			t.Fatal(v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground waiter not released")
	}
	cancel()
	if v, err := m.View("main", id); err != nil || v.Status != "running" {
		t.Fatal("promotion canceled command", v, err)
	}
	if len(m.Foreground("main")) != 0 {
		t.Fatal("promoted shell still foreground")
	}
	if err = os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-notices:
		if v.ID != id || v.Status != "completed" {
			t.Fatal(v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("background completion missing")
	}
	if v, err := m.Promote("main", id); err != nil || v.Status != "completed" {
		t.Fatal(v, err)
	}
	if v, err := m.Wait(context.Background(), "main", id, nil); err != nil || v.Status != "completed" {
		t.Fatal(v, err)
	}
	select {
	case v := <-notices:
		t.Fatal("completion notified twice", v)
	default:
	}
}

func TestPromoteBeforeWaitNotifiesCompletion(t *testing.T) {
	notices := make(chan Snapshot, 1)
	m := New(context.Background(), func(v Snapshot) { notices <- v })
	defer m.Close()
	id, err := m.Start("main", "sleep 30", t.TempDir(), 0, true, false, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Promote("main", id); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	pendingResult := m.jobs[id].foregroundResultPending
	m.mu.Unlock()
	if !pendingResult {
		t.Fatal("promotion lost original result reservation before waiter registration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if v, err := m.Wait(ctx, "main", id, nil); err != nil || v.Status != "running" {
		t.Fatal(v, err)
	}
	if _, err = m.Stop("main", id); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-notices:
		if !v.Background {
			t.Fatal("background flag missing", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("completion UI notice missing")
	}
}
