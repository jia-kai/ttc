package session

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/provider"
	"ttc/internal/tool"
)

func timerInspectionFixture(t *testing.T) (*Runtime, <-chan wakeup) {
	t.Helper()
	states := make(chan wakeup, 16)
	r := &Runtime{Tools: tool.NewRegistry()}
	r.timers = newWakeups(context.Background(), func(v wakeup) { states <- v })
	r.addWakeupTools()
	t.Cleanup(r.timers.close)
	return r, states
}

func invokeTimerTool(t *testing.T, r *Runtime, actor, name string, args map[string]any) map[string]any {
	t.Helper()
	input, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: actor, CallID: "timer-call"}, name, input)
	var result map[string]any
	if err := json.Unmarshal(record.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result["ok"] != true {
		t.Fatalf("%s failed: %s", name, record.Result)
	}
	return result
}

func assertTimerStartup(t *testing.T, detail string, input map[string]any) {
	t.Helper()
	_, fenced, ok := strings.Cut(detail, "### Original startup parameters\n\n")
	if !ok {
		t.Fatal("missing original startup parameters", detail)
	}
	opening, body, ok := strings.Cut(fenced, "\n")
	if !ok || !strings.HasSuffix(opening, "json") {
		t.Fatal("missing JSON fence", fenced)
	}
	closing := strings.TrimSuffix(opening, "json")
	body = strings.TrimSuffix(body, "\n"+closing+"\n")
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err, detail)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(encoded, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("startup parameters changed: got %#v, want %#v", got, want)
	}
}

func TestTimerDetailOriginalInputAndCancellation(t *testing.T) {
	for _, mode := range []string{"at", "delay_seconds"} {
		t.Run(mode, func(t *testing.T) {
			r, states := timerInspectionFixture(t)
			input := map[string]any{"name": "Original **timer**", "message": "Original reminder\n```keep the complete message```"}
			if mode == "at" {
				input[mode] = time.Now().Add(time.Hour).In(time.FixedZone("offset", 2*60*60)).Format(time.RFC3339Nano)
				input["repeat_seconds"] = 60
			} else {
				input[mode] = 60
			}
			actor := "main/child_research"
			result := invokeTimerTool(t, r, actor, "wakeup_schedule", input)
			id := result["wakeup_id"].(string)
			initial := <-states
			if initial.actor != actor || initial.startupText == "" || initial.Status != "scheduled" {
				t.Fatal("initial publication lacked immutable metadata", initial)
			}
			view, err := r.TimerDetail(id)
			detail := view.Header + view.Text
			if err != nil || view.Actor != actor || !strings.Contains(detail, "Status: scheduled") || !strings.Contains(detail, "Fired count: 0") || !strings.Contains(detail, "Next at: "+result["next_at"].(string)) || !strings.Contains(detail, "Countdown:") {
				t.Fatal(view, err)
			}
			startupText := view.Text
			if !strings.HasPrefix(view.Focus, "Status: scheduled\nCountdown:") || strings.Count(view.Focus, "\n") != 1 {
				t.Fatal("missing compact live timer header", view.Focus)
			}
			if startupText != initial.startupText || strings.Contains(view.Header, "**") || strings.Contains(view.Header, "Original startup parameters") || strings.Contains(view.Text, "Countdown:") {
				t.Fatal("startup text and live plain-text header were not separated", view)
			}
			assertTimerStartup(t, detail, input)
			invokeTimerTool(t, r, "main", "wakeup_cancel", map[string]any{"wakeup_id": id})
			view, err = r.TimerDetail(id)
			detail = view.Header + view.Text
			if err != nil || view.Actor != actor || !strings.Contains(detail, "Status: cancelled") || strings.Contains(detail, "Next at:") || strings.Contains(detail, "Countdown:") || view.Text != startupText {
				t.Fatal("cancelled timer lost its detail", view, err)
			}
			assertTimerStartup(t, detail, input)
			if view.Focus != "Status: cancelled" {
				t.Fatal("cancelled compact header retained countdown", view.Focus)
			}
		})
	}
}

func TestTimerDetailSurvivesFiring(t *testing.T) {
	for _, repeating := range []bool{false, true} {
		name := "one-shot"
		if repeating {
			name = "repeating"
		}
		t.Run(name, func(t *testing.T) {
			r, states := timerInspectionFixture(t)
			input := map[string]any{"name": name, "message": "Check the experiment", "delay_seconds": 0}
			if repeating {
				input["repeat_seconds"] = 60
			}
			actor := "main"
			result := invokeTimerTool(t, r, actor, "wakeup_schedule", input)
			id := result["wakeup_id"].(string)
			initial := <-states // Initial publication precedes firing.
			select {
			case fired := <-states:
				if fired.Fired != 1 || fired.actor != actor || fired.startupText != initial.startupText || fired.startupText == "" {
					t.Fatal(fired)
				}
			case <-time.After(time.Second):
				t.Fatal("timer did not fire")
			}
			r.timers.deliveredThrough(id, 1)
			view, err := r.TimerDetail(id)
			detail := view.Header + view.Text
			status := "fired"
			if repeating {
				status = "scheduled"
			}
			if err != nil || view.Actor != actor || !strings.Contains(detail, "Status: "+status) || !strings.Contains(detail, "Fired count: 1") || !strings.Contains(detail, "Last result: delivered") || strings.Contains(detail, "Countdown:") != repeating || view.Text != initial.startupText {
				t.Fatal(view, err)
			}
			assertTimerStartup(t, detail, input)
			if repeating {
				invokeTimerTool(t, r, actor, "wakeup_cancel", map[string]any{"wakeup_id": id})
				view, err = r.TimerDetail(id)
				detail = view.Header + view.Text
				if err != nil || view.Actor != actor || !strings.Contains(detail, "Status: cancelled") || !strings.Contains(detail, "Fired count: 1") || view.Text != initial.startupText {
					t.Fatal(view, err)
				}
				assertTimerStartup(t, detail, input)
			}
		})
	}
}

func TestTimerDetailNotFound(t *testing.T) {
	r, _ := timerInspectionFixture(t)
	for _, id := range []string{"", "missing-timer"} {
		view, err := r.TimerDetail(id)
		if err == nil || view != (TimerInspection{}) || !strings.Contains(err.Error(), "not found") {
			t.Fatal(view, err)
		}
	}
	if _, err := (&Runtime{}).TimerDetail("missing-timer"); err == nil {
		t.Fatal("uninitialized timer runtime accepted a missing ID")
	}
}

func TestTimerDetailCountdownOnlyChangesHeader(t *testing.T) {
	r, _ := timerInspectionFixture(t)
	input := map[string]any{"name": "countdown", "message": "Immutable reminder", "delay_seconds": 86400}
	result := invokeTimerTool(t, r, "main", "wakeup_schedule", input)
	id := result["wakeup_id"].(string)
	initial, err := r.TimerDetail(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		remaining time.Duration
		countdown string
	}{
		{time.Hour, "3600 seconds"},
		{90500 * time.Millisecond, "91 seconds"},
		{-time.Second, "due now"},
	} {
		// Advance the inspection deadline without sleeping or racing the timer's
		// goroutine, which is still waiting for the original distant deadline.
		r.timers.mu.Lock()
		v := r.timers.items[id]
		v.nextAt = time.Now().Add(tc.remaining)
		v.NextAt = v.nextAt.UTC().Format(time.RFC3339)
		r.timers.mu.Unlock()
		view, err := r.TimerDetail(id)
		if err != nil || view.Text != initial.Text || view.Actor != initial.Actor || view.Header == initial.Header || !strings.Contains(view.Header, "Countdown: "+tc.countdown+"\n") {
			t.Fatal("countdown refresh changed immutable text or lost current state", view, err)
		}
		assertTimerStartup(t, view.Header+view.Text, input)
		initial = view
	}
}

func TestTimerDetailOwnsStartupParameters(t *testing.T) {
	r, states := timerInspectionFixture(t)
	deadline := time.Now().Add(time.Hour)
	at, repeat := deadline.Format(time.RFC3339Nano), 60
	input := map[string]any{"name": "immutable", "message": "Original message", "at": at, "repeat_seconds": repeat}
	args := wakeupSchedule{Name: "immutable", Message: "Original message", At: &at, Repeat: &repeat}
	v, err := r.timers.schedule(args, "main/child_original", deadline)
	if err != nil {
		t.Fatal(err)
	}
	initial := <-states
	at, repeat = "changed", 1
	args.Name, args.Message = "changed", "changed"
	view, err := r.TimerDetail(v.ID)
	if err != nil || view.Actor != "main/child_original" {
		t.Fatal(view, err)
	}
	assertTimerStartup(t, view.Header+view.Text, input)
	if initial.startupText != v.startupText || initial.actor != v.actor || view.Text != initial.startupText {
		t.Fatal("initial publication did not own the same immutable metadata")
	}
}

func TestTimerDetailSurvivesCompaction(t *testing.T) {
	r, _ := runtimeFixture(t, []provider.ScriptResponse{{Text: "Earlier research completed."}})
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("Old experiment notes. ", 700))
	before, generation := r.Current(), r.Generation()
	input := map[string]any{"name": "keep-inspectable", "message": "Review experiment progress", "delay_seconds": 3600, "repeat_seconds": 60}
	actor := "main/child_experiment"
	result := invokeTimerTool(t, r, actor, "wakeup_schedule", input)
	id := result["wakeup_id"].(string)
	if _, err := r.compactContext(context.Background(), "", r.CurrentSelection(), nil); err != nil {
		t.Fatal(err)
	}
	if r.Current() == before || r.Generation() != generation {
		t.Fatal("compaction did not preserve the live generation")
	}
	view, err := r.TimerDetail(id)
	detail := view.Header + view.Text
	if err != nil || view.Actor != actor || !strings.Contains(detail, "Status: scheduled") || !strings.Contains(detail, "Fired count: 0") || !strings.Contains(detail, "Next at: "+result["next_at"].(string)) {
		t.Fatal(view, err)
	}
	assertTimerStartup(t, detail, input)
}
