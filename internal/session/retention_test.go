package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/history"
	"ttc/internal/llm"
)

func retentionFixture(t *testing.T) (*Runtime, chan Event, string) {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	selection := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
	path, id := t.TempDir(), history.NewID("session")
	turn, _, err := store.StartSession(id, path, selection, llm.Message{Role: "user", Content: "Loaded conversation"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 16)
	r := &Runtime{Store: store, ctx: ctx, current: id, persisted: true, selection: selection, Emit: func(event Event) { events <- event }}
	return r, events, path
}

func TestRetentionStartupProtectsLoadedLineageAndStops(t *testing.T) {
	r, _, path := retentionFixture(t)
	other := history.NewID("session")
	turn, _, err := r.Store.StartSession(other, path, r.CurrentSelection(), llm.Message{Role: "user", Content: "Expired conversation"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.DB.Exec("UPDATE sessions SET last_activity_ms=?", time.Now().Add(-31*24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	stop := r.startRetention()
	t.Cleanup(stop)
	deadline := time.Now().Add(2 * time.Second)
	for {
		var count int
		if err := r.Store.DB.QueryRow("SELECT count(*) FROM sessions WHERE id=?", other).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup retention did not expire old history")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := r.Store.Session(r.Current()); err != nil {
		t.Fatal("startup retention deleted loaded lineage", err)
	}
	stop()
	stop()
}

func TestRetentionCleanupErrorsAreVisibleAndShutdownJoins(t *testing.T) {
	r, events, _ := retentionFixture(t)
	out := t.TempDir()
	if err := os.Symlink(out, filepath.Join(r.Store.Root, "lineages")); err != nil {
		t.Fatal(err)
	}
	stop := r.startRetention()
	t.Cleanup(stop)
	select {
	case event := <-events:
		if event.Kind != "status" || !strings.HasPrefix(event.Text, "History cleanup failed:") {
			t.Fatal("cleanup failure was not rendered", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup failure not reported")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("retention shutdown did not join the worker")
	}
}
