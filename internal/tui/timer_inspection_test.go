package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

func TestSidebarTimerInspectionShowsStartupAndSourceAfterChildCloses(t *testing.T) {
	for _, child := range []bool{false, true} {
		name := "Main agent"
		responses := []llm.ScriptResponse{}
		if child {
			name = "timer helper"
			responses = append(responses, llm.ScriptResponse{Calls: []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":false,"prompt":"schedule timer","label":"timer helper"}`)}}})
		}
		responses = append(responses,
			llm.ScriptResponse{Calls: []llm.ToolCall{{ID: "schedule", Name: "wakeup_schedule", Arguments: []byte(`{"name":"inspect-timer","message":"Check the detailed timer parameters.","delay_seconds":86400,"repeat_seconds":180}`)}}},
			llm.ScriptResponse{Text: "Timer scheduled."})
		if child {
			responses = append(responses, llm.ScriptResponse{Text: "Parent finished."})
		}
		t.Run(name, func(t *testing.T) {
			u := newQuestionTestUI(t, &llm.Script{Responses: responses})
			u.screen.SetSize(180, 60)
			u.screen.PostEventWait(tcell.NewEventResize(180, 60))
			u.typeText("schedule a timer")
			u.key(tcell.KeyEnter)
			frame := u.wait(t, "Turn complete")
			if child && len(u.runtime.ChildViews("main")) != 0 {
				t.Fatal("fixture child did not close")
			}
			timers := u.runtime.LiveTimers()
			if len(timers) != 1 {
				t.Fatal("missing timer", timers)
			}
			row := -1
			for y, text := range strings.Split(frame, "\n") {
				if strings.Contains(text, "inspect-timer") && strings.Contains(text, " · in ") {
					row = y
					break
				}
			}
			if row < 0 {
				t.Fatal("timer missing from sidebar", frame)
			}
			u.screen.PostEventWait(tcell.NewEventMouse(150, row, tcell.Button1, 0))
			u.wait(t, "Source agent: "+name)
			for _, want := range []string{"delay_seconds", "86400", "repeat_seconds", "180", "Check the detailed timer parameters.", timers[0].ID} {
				u.wait(t, want)
			}
			args, err := json.Marshal(map[string]string{"wakeup_id": timers[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			record := u.runtime.Tools.Invoke(context.Background(), tool.Execution{Actor: "main", SessionID: u.runtime.Current()}, "wakeup_cancel", args)
			if !strings.Contains(string(record.Result), `"ok":true`) {
				t.Fatal("cancel failed", record)
			}
			u.wait(t, "cancelled")
			u.wait(t, "Source agent: "+name)
		})
	}
}

func TestInspectorSourceAgentDoesNotReplaceViewHeader(t *testing.T) {
	w := Window{SourceAgent: "timer helper", Header: "View-specific information"}
	if got := strings.Join(w.HeaderLines(80, 5), "\n"); got != "Source agent: timer helper\nView-specific information" {
		t.Fatal("source attribution replaced header", got)
	}
}

func TestInspectorSourceAgentUsesOneRowInNarrowPane(t *testing.T) {
	w := Window{SourceAgent: strings.Repeat("界", 64), Text: "Body content"}
	for _, width := range []int{1, 8, 20} {
		lines := w.HeaderLines(width, 5)
		if len(lines) != 1 || ansi.StringWidth(lines[0]) > width {
			t.Fatal("source agent expanded beyond one row", width, lines)
		}
	}
	_, height := windowContentSize(24, 10, &w)
	if height != 5 {
		t.Fatal("source attribution consumed body viewport", height)
	}
}

func TestInspectionSourcePolicy(t *testing.T) {
	for _, test := range []struct {
		kind, eventType string
		want            bool
	}{{"tool_result", "", true}, {"tool_call", "", true}, {"status", "job_completion", true}, {"message", "", false}, {"status", "system_prompt", false}} {
		if got := inspectionShowsSource(test.kind, test.eventType); got != test.want {
			t.Fatal("inconsistent inspection attribution", test, got)
		}
	}
}

func TestTimerHeaderPreservesCountdownAndBodyInShortPane(t *testing.T) {
	w := Window{TimerID: "wake_example", SourceAgent: "Main agent",
		Header:      "ID: wake_example\nStatus: scheduled\nFired count: 0\nNext at: 2026-10-06T16:00:00Z\nCountdown: 3600 seconds\n",
		HeaderFocus: "Status: scheduled\nCountdown: 3600 seconds", Text: "Original startup parameters"}
	for _, size := range [][2]int{{80, 10}, {40, 12}} {
		width, height := windowContentSize(size[0], size[1], &w)
		_, _, _, outerHeight := windowBounds(size[0], size[1])
		lines := w.HeaderLines(width, outerHeight-3)
		header := strings.Join(lines, "\n")
		if height < 3 || !strings.Contains(header, "Status: scheduled") || !strings.Contains(header, "Countdown: 3600 seconds") || len(lines) != 3 {
			t.Fatal("compact timer header hid live state or scrolling body", size, height, header)
		}
	}
}

func TestTimerHeaderRefreshKeepsMarkdownLayoutCached(t *testing.T) {
	w := Window{Text: "### Original startup parameters\n\n```json\n{\"message\":\"unchanged\"}\n```", Markdown: true, Header: "Countdown: 3 seconds"}
	w.Lines(80, 20)
	if len(w.cachedLines) == 0 {
		t.Fatal("missing inspector layout")
	}
	first := &w.cachedLines[0]
	w.Header = "Countdown: 2 seconds"
	w.Lines(80, 20)
	if first != &w.cachedLines[0] {
		t.Fatal("countdown refresh rebuilt immutable Markdown layout")
	}
}

func TestOpenTimerInspectorCountsDownAndSurvivesOneShotFiring(t *testing.T) {
	p := &llm.Script{Responses: []llm.ScriptResponse{
		{Calls: []llm.ToolCall{{ID: "schedule", Name: "wakeup_schedule", Arguments: []byte(`{"name":"one-shot-inspect","message":"Firing observed.","delay_seconds":4}`)}}},
		{Text: "Timer scheduled."}, {Text: "Timer fired."},
	}}
	u := newQuestionTestUI(t, p)
	u.screen.SetSize(180, 60)
	u.screen.PostEventWait(tcell.NewEventResize(180, 60))
	u.typeText("schedule a timer")
	u.key(tcell.KeyEnter)
	frame := u.wait(t, "Turn complete")
	row := -1
	for y, text := range strings.Split(frame, "\n") {
		if strings.Contains(text, "one-shot-inspect") && strings.Contains(text, " · in ") {
			row = y
			break
		}
	}
	if row < 0 {
		t.Fatal("one-shot timer missing", frame)
	}
	u.screen.PostEventWait(tcell.NewEventMouse(150, row, tcell.Button1, 0))
	frame = u.wait(t, "Countdown:")
	countdown := func(frame string) string {
		for _, line := range strings.Split(frame, "\n") {
			if strings.Contains(line, "Countdown:") {
				return line
			}
		}
		return ""
	}
	first := countdown(frame)
	deadline := time.After(6 * time.Second)
	changed := false
	for {
		select {
		case frame = <-u.screen.frames:
			if current := countdown(frame); current != "" && current != first {
				changed = true
			}
			if strings.Contains(frame, "Status: fired") {
				if !changed || !strings.Contains(frame, "Source agent: Main agent") || !strings.Contains(frame, "Original startup parameters") {
					t.Fatal("timer inspector lost countdown, attribution or parameters", changed, frame)
				}
				return
			}
		case <-deadline:
			t.Fatal("open timer inspector did not reflect firing", frame)
		}
	}
}
