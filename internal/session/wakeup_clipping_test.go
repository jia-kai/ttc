package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"ttc/internal/tool"
)

func TestWakeupDurationBoundaryClipping(t *testing.T) {
	const maximum = 31536000
	for _, spec := range []struct {
		tool, parameter string
		minimum         int
	}{
		{"wakeup_schedule", "delay_seconds", 0},
		{"wakeup_schedule", "repeat_seconds", 1},
		{"wakeup_update", "delay_seconds", 0},
		{"wakeup_update", "repeat_seconds", 0},
	} {
		for _, requested := range []int{spec.minimum - 1, spec.minimum, maximum, maximum + 1} {
			t.Run(fmt.Sprintf("%s/%s/%d", spec.tool, spec.parameter, requested), func(t *testing.T) {
				r, states := timerInspectionFixture(t)
				args := map[string]any{"name": "boundary"}
				if spec.tool == "wakeup_update" {
					if _, err := r.timers.schedule(wakeupSchedule{Name: "boundary", Message: "check"}, "main", time.Now().Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
					timerState(t, states)
				} else {
					args["message"] = "check"
					args["delay_seconds"] = 3600 // Recurring reminders never fire during the test.
				}
				args[spec.parameter] = requested
				raw, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				original := bytes.Clone(raw)
				before := time.Now()
				record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main"}, spec.tool, raw)
				after := time.Now()
				if !bytes.Equal(raw, original) || !bytes.Equal(record.Arguments, original) {
					t.Fatalf("original arguments changed: input=%s record=%s want=%s", raw, record.Arguments, original)
				}
				var result struct {
					OK       bool `json:"ok"`
					Warnings []struct {
						Code      string `json:"code"`
						Parameter string `json:"parameter"`
						Requested int    `json:"requested"`
						Effective int    `json:"effective"`
						Minimum   int    `json:"minimum"`
						Maximum   int    `json:"maximum"`
						Message   string `json:"message"`
					} `json:"warnings"`
				}
				if err := json.Unmarshal(record.Result, &result); err != nil {
					t.Fatal(err)
				}
				if !result.OK {
					t.Fatalf("invocation failed: %s", record.Result)
				}
				effective := max(spec.minimum, min(requested, maximum))
				if effective == requested {
					if len(result.Warnings) != 0 {
						t.Fatalf("in-range input warned: %s", record.Result)
					}
				} else {
					if len(result.Warnings) != 1 {
						t.Fatalf("want one clipping warning: %s", record.Result)
					}
					w := result.Warnings[0]
					if w.Code != "input_clipped" || w.Parameter != spec.parameter || w.Requested != requested || w.Effective != effective || w.Minimum != spec.minimum || w.Maximum != maximum || w.Message == "" {
						t.Fatalf("incorrect warning metadata: %s", record.Result)
					}
				}
				// Inspect the published execution snapshot, not merely the response.
				// Zero-delay cases are one-shot, so a subsequent firing is harmless.
				state := timerState(t, states)
				if spec.parameter == "repeat_seconds" {
					if state.Repeat != effective || !state.nextAt.After(before.Add(59*time.Minute)) {
						t.Fatalf("execution did not use clipped repeat/future deadline: %+v", state)
					}
				} else {
					delay := time.Duration(effective) * time.Second
					if state.nextAt.Before(before.Add(delay)) || state.nextAt.After(after.Add(delay)) || state.Repeat != 0 {
						t.Fatalf("execution did not use clipped delay: next=%s window=[%s,%s] repeat=%d", state.nextAt, before.Add(delay), after.Add(delay), state.Repeat)
					}
				}
			})
		}
	}
}

func TestWakeupOptionalRepeatClippingSemantics(t *testing.T) {
	r, states := timerInspectionFixture(t)
	result := invokeTimerTool(t, r, "main", "wakeup_schedule", map[string]any{"name": "once", "message": "check", "delay_seconds": 3600})
	if state := timerState(t, states); state.Repeat != 0 || result["warnings"] != nil {
		t.Fatalf("omitted schedule repeat changed: %+v result=%v", state, result)
	}
	initial, err := r.timers.schedule(wakeupSchedule{Name: "repeat", Message: "check", Repeat: updatePtr(60)}, "main", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	timerState(t, states)
	result = invokeTimerTool(t, r, "main", "wakeup_update", map[string]any{"name": "repeat", "message": "changed"})
	if state := timerState(t, states); state.Repeat != 60 || state.nextAt != initial.nextAt || result["warnings"] != nil {
		t.Fatalf("omitted update repeat changed: %+v result=%v", state, result)
	}
	result = invokeTimerTool(t, r, "main", "wakeup_update", map[string]any{"name": "repeat", "repeat_seconds": 0})
	if state := timerState(t, states); state.Repeat != 0 || state.nextAt != initial.nextAt || result["warnings"] != nil {
		t.Fatalf("explicit zero no longer disables repetition: %+v result=%v", state, result)
	}
}
