package render

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestSubagentBadgeNameAndCellBounds(t *testing.T) {
	for _, name := range []string{"research", strings.Repeat("x", 26), strings.Repeat("界", 27), "one\x1btwo\nthree"} {
		full := SubagentBadge("child-a", name, 100, false)
		if strings.ContainsAny(full, "\x1b\n") || !utf8.ValidString(full) {
			t.Fatal(full)
		}
		if utf8.RuneCountInString(name) == 27 && full != "["+strings.Repeat("界", 23)+"...]" {
			t.Fatal("name truncation", full)
		}
		for _, width := range []int{1, 2, 8, 26, 80} {
			colored := SubagentBadge("child-a", name, width, true)
			plain := SubagentBadge("child-a", name, width, false)
			if ansi.Strip(colored) != plain || ansi.StringWidth(colored) > width || !utf8.ValidString(colored) {
				t.Fatal(width, colored, plain)
			}
			if colored != SubagentBadge("child-a", name, width, true) || !strings.Contains(colored, "\x1b[38;2;") {
				t.Fatal("unstable or absent color", colored)
			}
		}
	}
}

func TestSuccessfulToolStatusPresentation(t *testing.T) {
	md := Tool("shell", []byte(`{"command":"true"}`), []byte(`{"status":"completed","exit_code":0}`))
	if !strings.Contains(md.Summary, "done") || !strings.Contains(md.Detail, "done") || strings.Contains(md.Detail, "completed") {
		t.Fatal(md)
	}
	for _, status := range []string{"running", "failed", "cancelled", "interrupted"} {
		if Status(status) != status {
			t.Fatal(status)
		}
	}
}

func TestSubagentResponseSingleLineAndToolFinalAnswer(t *testing.T) {
	for _, width := range []int{1, 2, 8, 40, 80} {
		row := SubagentResponse("child", "helper", "assistant", "hello\n界\tworld\r\x00\u2028next"+strings.Repeat("long", 1000), width, false)
		if strings.ContainsAny(row, "\n\r\t\u2028") || !utf8.ValidString(row) || ansi.StringWidth(row) > width {
			t.Fatal(width, row)
		}
	}
	md := Tool("subagent", []byte(`{"label":"helper"}`), []byte(`{"assignment_status":"completed","answer":"first\nsecond\nFULL END","stdout":"unchanged tool output"}`))
	if !strings.Contains(md.Summary, "final answer first second FULL END ...") || strings.Contains(md.Summary, "stdout") || !strings.Contains(md.Detail, "FULL END") || !strings.Contains(md.Detail, "unchanged tool output") {
		t.Fatal(md)
	}
}

func TestSubagentJobCompletionDoesNotRepeatAssistantOutputTail(t *testing.T) {
	result := []byte(`{"status":"completed","stdout":"hidden assistant TAIL","stderr":"diagnostic"}`)
	child := Tool("subagent", []byte(`{"label":"helper"}`), []byte(`{"assignment_status":"completed","stdout":"hidden assistant TAIL","stderr":"diagnostic"}`))
	if strings.Contains(child.Summary, "TAIL") || !strings.Contains(child.Detail, "TAIL") || !strings.Contains(child.Summary, "diagnostic") {
		t.Fatal(child)
	}
	shell := Tool("shell", []byte(`{"command":"fixture"}`), result)
	if !strings.Contains(shell.Summary, "TAIL") {
		t.Fatal("ordinary tool output changed", shell)
	}
}

func TestSubagentAssignmentStatusPresentation(t *testing.T) {
	for _, status := range []string{"running", "completed", "failed", "cancelled"} {
		for _, answer := range []string{"", `,"answer":"first\nsecond"`} {
			for name, renderer := range map[string]func(string, json.RawMessage, json.RawMessage) Markdown{
				"tool":      Tool,
				"inspected": InspectedTool,
			} {
				t.Run(status+answer+name, func(t *testing.T) {
					md := renderer("subagent", []byte(`{"label":"helper"}`), []byte(`{"assignment_status":"`+status+`"`+answer+`}`))
					if !strings.Contains(md.Summary, " · "+Status(status)) || strings.Contains(md.Summary, "nil") || !strings.Contains(md.Detail, "**Assignment status:** "+inlineCode(Status(status))) {
						t.Fatal(md)
					}
				})
			}
		}
	}
}

func TestSubagentSummaryUsesOnlyAssignmentStatus(t *testing.T) {
	for _, tc := range []struct {
		name, result, want string
	}{
		{"completed", `{"assignment_status":"completed","status":"running","answer":"answer"}`, "**subagent** · done · final answer answer ..."},
		{"background", `{"assignment_status":"running","job_id":"job"}`, "**subagent** · started · ` job `"},
		{"missing", `{"answer":"answer"}`, "**subagent** · final answer answer ..."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := Tool("subagent", []byte(`{"background":true}`), []byte(tc.result))
			if md.Summary != tc.want {
				t.Fatalf("summary = %q, want %q", md.Summary, tc.want)
			}
		})
	}
}
