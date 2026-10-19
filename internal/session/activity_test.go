package session

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"ttc/internal/history"
	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

func TestActivityConstructorRefreshesLoadedSession(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Loaded conversation")
	if _, err := historytest.DB(t, r.Store).Exec("UPDATE sessions SET last_activity_ms=1"); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UnixMilli()
	loaded, err := New(context.Background(), r.Store, r.Workspace, r.Provider, r.CurrentSelection(), r.Current(), r.Skills, tool.WebSearchConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if activity := sessionActivity(t, r, loaded.Current()); activity < before {
		t.Fatal("constructor did not refresh loaded session", activity)
	}
}

func TestActivityRefreshOnlyTouchesLoadedSession(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	r.refreshLoadedActivity(r.ctx, time.Now())
	for _, table := range []string{"sessions", "entries", "model_requests", "turns"} {
		var count int
		if err := historytest.DB(t, r.Store).QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("activity refresh persisted %s: count=%d err=%v", table, count, err)
		}
	}
	if len(events) != 0 {
		t.Fatal("blank session refresh emitted a status")
	}

	seedRuntime(t, r, "Loaded conversation")
	other := history.NewID("session")
	turn, _, err := r.Store.StartSession(other, r.Workspace.Root, r.CurrentSelection(), llm.Message{Type: llm.TypeUserInput, Content: "Other conversation"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := historytest.DB(t, r.Store).Exec("UPDATE sessions SET last_activity_ms=1"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r.refreshLoadedActivity(r.ctx, now)
	if activity := sessionActivity(t, r, r.Current()); activity != now.UnixMilli() {
		t.Fatal("loaded session activity was not refreshed", activity)
	}
	if activity := sessionActivity(t, r, other); activity != 1 {
		t.Fatal("unloaded session activity changed", activity)
	}
	if len(events) != 0 {
		t.Fatal("activity refresh emitted a status on success")
	}
}

func TestActivityLifecycleAndHeartbeatWithoutScanningHistory(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	seedRuntime(t, r, "Loaded conversation")
	r.activityStop()
	r.activityStop = nil
	// The heartbeat's context belongs to the virtual-time bubble.
	r.ctx = context.Background()
	// A statistics query would fail, while session updates remain valid.
	if _, err := historytest.DB(t, r.Store).Exec("ALTER TABLE entries RENAME TO hidden_entries; UPDATE sessions SET last_activity_ms=1"); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UnixMilli()
	// Even a failed lifecycle command refreshes the still-loaded session.
	if _, err := r.Command("/load missing-session"); err == nil {
		t.Fatal("load unexpectedly succeeded")
	}
	if activity := sessionActivity(t, r, r.Current()); activity < before {
		t.Fatal("lifecycle command did not refresh loaded activity", activity)
	}
	if len(events) != 0 {
		t.Fatal("lifecycle command emitted storage statistics", len(events))
	}

	synctest.Test(t, func(t *testing.T) {
		// Virtual time starts before the lifecycle command's wall-clock timestamp.
		if _, err := historytest.DB(t, r.Store).Exec("UPDATE sessions SET last_activity_ms=1"); err != nil {
			t.Fatal(err)
		}
		stop := r.startActivityRefresh()
		defer stop()
		for i := 0; i < 2; i++ {
			synctest.Wait()
			time.Sleep(time.Hour)
			synctest.Wait()
			if activity := sessionActivity(t, r, r.Current()); activity != time.Now().UnixMilli() {
				t.Fatal("heartbeat did not refresh loaded activity", activity)
			}
			if len(events) != 0 {
				t.Fatal("heartbeat scanned history or published statistics")
			}
		}
		stop()
		stop()
		before := sessionActivity(t, r, r.Current())
		time.Sleep(time.Hour)
		if activity := sessionActivity(t, r, r.Current()); activity != before {
			t.Fatal("stopped heartbeat refreshed activity", activity)
		}
	})
}

func TestActivityRefreshReportsErrorsUnlessCanceled(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	seedRuntime(t, r, "Loaded conversation")
	if _, err := historytest.DB(t, r.Store).Exec(`CREATE TRIGGER reject_activity_refresh BEFORE UPDATE OF last_activity_ms ON sessions
		BEGIN SELECT RAISE(ABORT,'activity refresh denied'); END`); err != nil {
		t.Fatal(err)
	}
	r.refreshLoadedActivity(r.ctx, time.Now())
	select {
	case event := <-events:
		if event.Kind != "status" || !strings.HasPrefix(event.Text, "History activity refresh failed:") || !strings.Contains(event.Text, "activity refresh denied") {
			t.Fatal("activity failure was not reported", event)
		}
	default:
		t.Fatal("activity failure was not reported")
	}
	ctx, cancel := context.WithCancel(r.ctx)
	cancel()
	r.refreshLoadedActivity(ctx, time.Now())
	if len(events) != 0 {
		t.Fatal("canceled activity refresh reported an error")
	}
}

func sessionActivity(t *testing.T, r *Runtime, id string) int64 {
	t.Helper()
	var activity int64
	if err := historytest.DB(t, r.Store).QueryRow("SELECT last_activity_ms FROM sessions WHERE id=?", id).Scan(&activity); err != nil {
		t.Fatal(err)
	}
	return activity
}
