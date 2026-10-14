package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ttc/internal/tool"
)

func updatePtr[T any](v T) *T { return &v }

func timerState(t *testing.T, states <-chan wakeup) wakeup {
	t.Helper()
	select {
	case v := <-states:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("timer state was not published")
		return wakeup{}
	}
}

func TestWakeupUpdatePreservesIdentityAndStartup(t *testing.T) {
	r, states := timerInspectionFixture(t)
	initial, err := r.timers.schedule(wakeupSchedule{Name: "check", Message: "old", Repeat: updatePtr(60)}, "child", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	timerState(t, states)
	result := invokeTimerTool(t, r, "main", "wakeup_update", map[string]any{"name": "check", "message": "new", "repeat_seconds": 0})
	updated := timerState(t, states)
	if updated.ID != initial.ID || updated.actor != initial.actor || updated.nextAt != initial.nextAt || updated.Fired != 0 || updated.Repeat != 0 || updated.Message != "new" || updated.startupText != initial.startupText || result["wakeup_id"] != initial.ID {
		t.Fatalf("update lost identity/state: %+v", updated)
	}
	detail, err := r.TimerDetail(initial.ID)
	if err != nil || !strings.Contains(detail.Header, "Message: new") || !strings.Contains(detail.Header, "Repeat seconds: 0") || detail.Text != initial.startupText {
		t.Fatal(detail, err)
	}
	if _, err := r.timers.schedule(wakeupSchedule{Name: "check", Message: "duplicate"}, "main", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("update allowed duplicate active name")
	}
}

func TestWakeupUpdateReschedulesAndStopsRepetition(t *testing.T) {
	r, states := timerInspectionFixture(t)
	initial, err := r.timers.schedule(wakeupSchedule{Name: "check", Message: "old", Repeat: updatePtr(60)}, "main", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	timerState(t, states)
	if _, err := r.timers.update(wakeupUpdate{ID: &initial.ID, Delay: updatePtr(0)}, "main"); err != nil {
		t.Fatal(err)
	}
	timerState(t, states) // Atomic update publication precedes the firing.
	fired := timerState(t, states)
	if fired.Status != "scheduled" || fired.Fired != 1 || fired.Message != "old" {
		t.Fatal(fired)
	}
	r.timers.deliveredThrough(initial.ID, 1)
	updated, err := r.timers.update(wakeupUpdate{ID: &initial.ID, Delay: updatePtr(0), Repeat: updatePtr(0), Message: updatePtr("new")}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Fired != 1 || updated.Last != "delivered" {
		t.Fatal(updated)
	}
	timerState(t, states)
	last := timerState(t, states)
	if last.Status != "fired" || last.Fired != 2 || last.Message != "new" || last.NextAt != "" {
		t.Fatal(last)
	}
	// Published firings are value snapshots, not pointers into mutable state.
	if fired.Message != "old" || fired.Repeat != 60 {
		t.Fatal("committed firing changed", fired)
	}
	if _, err := r.timers.update(wakeupUpdate{ID: &initial.ID, Message: updatePtr("late")}, "main"); err == nil {
		t.Fatal("updated fired timer")
	}
}

func TestWakeupUpdateInvalidIsAtomic(t *testing.T) {
	r, states := timerInspectionFixture(t)
	initial, err := r.timers.schedule(wakeupSchedule{Name: "check", Message: "old"}, "child", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	timerState(t, states)
	cases := []wakeupUpdate{
		{ID: &initial.ID},
		{ID: &initial.ID, Name: updatePtr("check"), Message: updatePtr("new")},
		{ID: &initial.ID, At: updatePtr("bad"), Message: updatePtr("new")},
		{ID: &initial.ID, At: updatePtr(time.Now().Format(time.RFC3339)), Delay: updatePtr(1)},
		{ID: &initial.ID, Repeat: updatePtr(-1), Message: updatePtr("new")},
		{ID: &initial.ID, Message: updatePtr("")},
	}
	for _, a := range cases {
		if _, err := r.timers.update(a, "main"); err == nil {
			t.Fatalf("accepted invalid update %+v", a)
		}
	}
	if _, err := r.timers.update(wakeupUpdate{ID: &initial.ID, Message: updatePtr("new")}, "other-child"); err == nil {
		t.Fatal("cross-child update allowed")
	}
	current := r.timers.list()[0]
	if current.Message != initial.Message || current.nextAt != initial.nextAt || current.Repeat != initial.Repeat {
		t.Fatal("failed update mutated timer", current)
	}
	select {
	case v := <-states:
		t.Fatal("failed update published", v)
	default:
	}
	if _, err := r.timers.stop(initial.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.timers.update(wakeupUpdate{ID: &initial.ID, Message: updatePtr("late")}, "main"); err == nil {
		t.Fatal("updated cancelled timer")
	}
}

func TestWakeupUpdateToolValidation(t *testing.T) {
	r, _ := timerInspectionFixture(t)
	for _, args := range []string{`{}`, `{"name":"missing","repeat_seconds":0}`, `{"name":"x","delay_seconds":-1}`, `{"name":"x","repeat_seconds":31536001}`, `{"name":"x","message":"a","unknown":1}`} {
		record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main"}, "wakeup_update", []byte(args))
		var result map[string]any
		if err := json.Unmarshal(record.Result, &result); err != nil {
			t.Fatal(err)
		}
		if result["ok"] != false {
			t.Fatal("accepted invalid/missing timer", args, string(record.Result))
		}
	}
}

func TestWakeupUpdateReadyOldTimerDoesNotFire(t *testing.T) {
	// Hold the mutation lock across the old deadline. Update it before allowing
	// the worker to commit; even a ready old timer must respect the new deadline.
	states := make(chan wakeup, 8)
	w := newWakeups(context.Background(), func(v wakeup) { states <- v })
	t.Cleanup(w.close)
	initial, err := w.schedule(wakeupSchedule{Name: "check", Message: "old"}, "main", time.Now().Add(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	timerState(t, states)
	w.mu.Lock()
	time.Sleep(40 * time.Millisecond)
	v := w.items[initial.ID]
	v.nextAt = time.Now().Add(time.Hour)
	v.NextAt = v.nextAt.UTC().Format(time.RFC3339)
	v.changed <- struct{}{}
	w.mu.Unlock()
	select {
	case v := <-states:
		t.Fatal("stale timer fired", v)
	case <-time.After(60 * time.Millisecond):
	}
	if got := w.list()[0]; got.Fired != 0 || got.Status != "scheduled" {
		t.Fatal(got)
	}
}
