package session

import (
	"context"
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
	turn, _, err := store.StartSession(id, path, selection, llm.Message{Type: llm.TypeUserInput, Content: "Loaded conversation"})
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

func retentionEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("history maintenance did not report a status")
		return Event{}
	}
}

func TestRetentionSmallStoreKeepsOldHistoryRefreshesActivityAndStops(t *testing.T) {
	r, events, path := retentionFixture(t)
	other := history.NewID("session")
	turn, _, err := r.Store.StartSession(other, path, r.CurrentSelection(), llm.Message{Type: llm.TypeUserInput, Content: "Old conversation in a small store"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-31 * 24 * time.Hour).UnixMilli()
	if _, err := r.Store.DB.Exec("UPDATE sessions SET last_activity_ms=?", old); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UnixMilli()
	stop := r.startRetention()
	t.Cleanup(stop)
	event := retentionEvent(t, events)
	if event.Kind != "status" || event.EntryID != 0 || !strings.HasPrefix(event.Text, "History database:") || !strings.Contains(event.Text, "4 entries") || strings.Contains(event.Text, "removed") {
		t.Fatal("small-store startup status", event)
	}
	for _, id := range []string{r.Current(), other} {
		saved, err := r.Store.Session(id)
		if err != nil {
			t.Fatal("small-store maintenance deleted history", err)
		}
		if id == r.Current() && saved.LastActivityMS < before {
			t.Fatal("maintenance did not refresh loaded activity below the size limit")
		}
		if id == other && saved.LastActivityMS != old {
			t.Fatal("maintenance changed unrelated activity")
		}
	}
	stop()
	stop()
}

func TestRetentionMaintenanceErrorsAreVisibleAndShutdownJoins(t *testing.T) {
	r, events, _ := retentionFixture(t)
	if err := r.Store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	stop := r.startRetention()
	t.Cleanup(stop)
	event := retentionEvent(t, events)
	if event.Kind != "status" || !strings.HasPrefix(event.Text, "History maintenance failed:") {
		t.Fatal("maintenance failure was not rendered", event)
	}
	stopped := make(chan struct{})
	go func() { stop(); stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("retention shutdown did not join the worker")
	}
}

func TestRetentionStartupDoesNotPersistBlankConversationOrModelInput(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	event := retentionEvent(t, events)
	if event.Kind != "status" || event.EntryID != 0 || !strings.Contains(event.Text, "0 entries · oldest n/a (empty history)") {
		t.Fatal("blank-store startup status", event)
	}
	for _, table := range []string{"sessions", "entries", "model_requests", "turns"} {
		var count int
		if err := r.Store.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("startup status persisted %s: count=%d err=%v", table, count, err)
		}
	}
	if entries, err := r.Entries(); err != nil || len(entries) != 0 {
		t.Fatal("startup status entered conversation history", entries, err)
	}
}

func TestRetentionRestartsRefreshActivityWithoutScanningOrDelayingFullPass(t *testing.T) {
	r, events, path := retentionFixture(t)
	other := history.NewID("session")
	turn, _, err := r.Store.StartSession(other, path, r.CurrentSelection(), llm.Message{Type: llm.TypeUserInput, Content: "Next loaded conversation"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	stop := r.startRetention()
	retentionEvent(t, events)
	stop()
	due := r.retentionDue // Ownership transfers only after joining the worker.
	if due.IsZero() {
		t.Fatal("startup did not schedule the next full attempt")
	}
	// Removing the statistics source makes any unintended count/min scan fail,
	// while session activity updates and their foreign keys remain valid.
	if _, err := r.Store.DB.Exec("ALTER TABLE entries RENAME TO hidden_entries"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.current = other
	r.mu.Unlock()
	for i := 0; i < 3; i++ {
		if _, err := r.Store.DB.Exec("UPDATE sessions SET last_activity_ms=1 WHERE id=?", other); err != nil {
			t.Fatal(err)
		}
		before := time.Now().UnixMilli()
		stop := r.startRetention()
		deadline := time.Now().Add(2 * time.Second)
		for {
			var activity int64
			if err := r.Store.DB.QueryRow("SELECT last_activity_ms FROM sessions WHERE id=?", other).Scan(&activity); err != nil {
				stop()
				t.Fatal(err)
			}
			if activity >= before {
				break
			}
			if time.Now().After(deadline) {
				stop()
				t.Fatal("restart did not refresh the newly loaded conversation")
			}
			time.Sleep(time.Millisecond)
		}
		stop()
		if r.retentionDue != due || len(events) != 0 {
			t.Fatalf("restart rescanned history or delayed maintenance: due=%v want=%v events=%d", r.retentionDue, due, len(events))
		}
	}
	if _, err := r.Store.DB.Exec("ALTER TABLE hidden_entries RENAME TO entries"); err != nil {
		t.Fatal(err)
	}
	r.maintainRetention(r.ctx, due.Add(-time.Nanosecond))
	if r.retentionDue != due || len(events) != 0 {
		t.Fatal("full maintenance ran before its original hourly deadline")
	}
	r.maintainRetention(r.ctx, due)
	if event := retentionEvent(t, events); !strings.HasPrefix(event.Text, "History database:") || r.retentionDue != due.Add(time.Hour) {
		t.Fatal("full maintenance did not run on its original hourly deadline", event, r.retentionDue)
	}
}

func TestRetentionFailedAttemptWaitsAnHourAndPreAttemptCancellationKeepsStartupDue(t *testing.T) {
	r, events, _ := retentionFixture(t)
	now := time.Now()
	canceled, cancel := context.WithCancel(r.ctx)
	cancel()
	r.maintainRetention(canceled, now)
	if !r.retentionDue.IsZero() || len(events) != 0 {
		t.Fatal("cancellation before the startup attempt consumed its slot")
	}
	if _, err := r.Store.DB.Exec("ALTER TABLE entries RENAME TO hidden_entries"); err != nil {
		t.Fatal(err)
	}
	r.maintainRetention(r.ctx, now)
	if event := retentionEvent(t, events); !strings.HasPrefix(event.Text, "History maintenance failed:") {
		t.Fatal("failed full maintenance was not reported", event)
	}
	if event := retentionEvent(t, events); !strings.HasPrefix(event.Text, "History storage statistics unavailable:") {
		t.Fatal("missing statistics source was not reported", event)
	}
	due := now.Add(time.Hour)
	if r.retentionDue != due {
		t.Fatal("failed attempt did not schedule an hourly retry", r.retentionDue)
	}
	if _, err := r.Store.DB.Exec("ALTER TABLE hidden_entries RENAME TO entries"); err != nil {
		t.Fatal(err)
	}
	r.maintainRetention(r.ctx, now.Add(time.Minute))
	if r.retentionDue != due || len(events) != 0 {
		t.Fatal("failed full maintenance retried before its hourly deadline")
	}
	r.maintainRetention(r.ctx, due)
	if event := retentionEvent(t, events); !strings.HasPrefix(event.Text, "History database:") {
		t.Fatal("hourly maintenance did not recover from a failed attempt", event)
	}
}

func TestRetentionMaintenanceWarningStillReportsStorageStatistics(t *testing.T) {
	r, events, _ := retentionFixture(t)
	if _, err := r.Store.DB.Exec(`CREATE TRIGGER reject_activity_refresh BEFORE UPDATE OF last_activity_ms ON sessions
		BEGIN SELECT RAISE(ABORT,'activity refresh denied'); END`); err != nil {
		t.Fatal(err)
	}
	r.maintainRetention(r.ctx, time.Now())
	warning := retentionEvent(t, events)
	if !strings.HasPrefix(warning.Text, "History maintenance failed:") || !strings.Contains(warning.Text, "activity refresh denied") {
		t.Fatal("maintenance warning not emitted before storage statistics", warning)
	}
	stats := retentionEvent(t, events)
	if !strings.HasPrefix(stats.Text, "History database:") || !strings.Contains(stats.Text, "2 entries · oldest 0.0 days") {
		t.Fatal("maintenance warning hid available startup storage statistics", stats)
	}
	if len(events) != 0 {
		t.Fatal("maintenance warning emitted duplicate statistics")
	}
}

func TestRetentionCanceledInitialAttemptRemainsDueUntilStartupStatistics(t *testing.T) {
	r, events, _ := retentionFixture(t)
	// Hold the sole SQL connection so maintenance reaches its first activity
	// update but cannot finish before this context expires.
	connection, err := r.Store.DB.Conn(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Millisecond)
	defer cancel()
	now := time.Now()
	r.maintainRetention(ctx, now)
	if ctx.Err() == nil || !r.retentionDue.IsZero() || len(events) != 0 {
		t.Fatal("canceled initial attempt suppressed the startup notice", ctx.Err(), r.retentionDue, len(events))
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	r.maintainRetention(r.ctx, now.Add(time.Minute))
	if event := retentionEvent(t, events); !strings.HasPrefix(event.Text, "History database:") || r.retentionDue.IsZero() {
		t.Fatal("replacement worker did not retry initial statistics", event, r.retentionDue)
	}
}

func TestHistoryStorageStatus(t *testing.T) {
	now := time.Unix(50*86400, 0)
	for _, tc := range []struct {
		name   string
		result history.MaintenanceResult
		want   string
	}{
		{"normal", history.MaintenanceResult{Stats: history.StorageStats{Bytes: 432327884, Entries: 83817, OldestMS: now.Add(-12*24*time.Hour - 9*time.Hour - 36*time.Minute).UnixMilli()}}, "History database: 412.3 MiB · 83,817 entries · oldest 12.4 days"},
		{"empty ignores timestamp", history.MaintenanceResult{Stats: history.StorageStats{Bytes: 1024, OldestMS: 1}}, "History database: 1.0 KiB · 0 entries · oldest n/a (empty history)"},
		{"unix epoch is not empty", history.MaintenanceResult{Stats: history.StorageStats{Bytes: 2 << 30, Entries: 1}}, "History database: 2.0 GiB · 1 entry · oldest 50.0 days"},
		{"future timestamp", history.MaintenanceResult{Stats: history.StorageStats{Bytes: 42, Entries: 2, OldestMS: now.Add(time.Hour).UnixMilli()}, RemovedLineages: 2}, "History database: 42.0 B · 2 entries · oldest 0.0 days · removed 2 inactive conversation families"},
		{"one removed family", history.MaintenanceResult{Stats: history.StorageStats{}, RemovedLineages: 1}, "History database: 0.0 B · 0 entries · oldest n/a (empty history) · removed 1 inactive conversation family"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := historyStorageStatus(tc.result, now); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}
