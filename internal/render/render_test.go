package render

import (
	"encoding/json"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTerminalControlsAndSafeFences(t *testing.T) {
	s := Clean("ok\x1b[31m\x00\u202eevil\n")
	if strings.ContainsAny(s, "\x1b\x00\u202e") {
		t.Fatal(s)
	}
	block := Fence("```\nvalue\n````", "text")
	if !strings.HasPrefix(block, "`````text\n") {
		t.Fatal(block)
	}
	inline := Inline(strings.Repeat("界", 100) + "[x]")
	if !utf8.ValidString(inline) || len(inline) > 160 {
		t.Fatal(inline)
	}
}

func TestToolCardsBoundPreviewAndKeepExactDetails(t *testing.T) {
	args := []byte(`{"command":"printf 'hello'; echo failure >&2"}`)
	result := []byte(`{"status":"completed","exit_code":7,"stdout":"hello\n","stderr":"failure\n","job_id":"job-test","truncated":false}`)
	md := Tool("shell", args, result)
	for _, text := range []string{"exit 7", "```sh", "**stdout**", "**stderr**", "hello", "failure"} {
		if !strings.Contains(md.Summary, text) {
			t.Fatal(text, md.Summary)
		}
	}
	if !strings.Contains(md.Detail, "echo failure >&2") || !strings.Contains(md.Detail, `**Exit code:** 7`) {
		t.Fatal(md.Detail)
	}
	long := strings.Repeat("界", 5000)
	b, _ := json.Marshal(map[string]any{"content": long, "path": "a.go", "lines": 3})
	md = Tool("read", []byte(`{"path":"a.go"}`), b)
	if len(md.Summary) > 4096 || !utf8.ValidString(md.Summary) || !strings.Contains(md.Detail, long) {
		t.Fatal("preview not bounded or lost detail")
	}
	if strings.Contains(md.Summary, long) || !strings.Contains(md.Summary, "a.go") {
		t.Fatal("read briefing should show its path", md.Summary)
	}
}

func TestGlobBriefingShowsOnlyPatternAndFailures(t *testing.T) {
	md := Tool("glob", []byte(`{"pattern":"**/*.go","path":"private/root"}`), []byte(`{"ok":true,"status":"running","paths":["result.go"],"root":"private/root"}`))
	plain, err := TerminalBriefing(md.Summary, 100, false)
	if err != nil || strings.Join(strings.Fields(plain), " ") != "glob · running · **/*.go" {
		t.Fatal(plain, err)
	}
	if !strings.Contains(md.Detail, "result.go") || !strings.Contains(md.Detail, "private/root") {
		t.Fatal(md.Detail)
	}
	md = Tool("glob", []byte(`{"pattern":"**/*.go"}`), []byte(`{"ok":false,"error":{"message":"root missing"}}`))
	plain, err = TerminalBriefing(md.Summary, 100, false)
	if err != nil || !strings.Contains(plain, "error: root missing") {
		t.Fatal(plain, err)
	}
}

func TestTerminalBriefingIsOneSafeHighlightedRow(t *testing.T) {
	md := Tool("shell", []byte(`{"command":"printf '界é'; printf done"}`), []byte(`{"status":"completed","exit_code":0,"stdout":"first\nlast\n","stderr":"warning\u001b[31m"}`))
	for _, width := range []int{1, 4, 12, 80, 250} {
		colored, err := TerminalBriefing(md.Summary, width, true)
		plain, plainErr := TerminalBriefing(md.Summary, width, false)
		if err != nil || plainErr != nil || strings.ContainsAny(colored, "\n\r\t") || strings.ContainsAny(plain, "\n\r\t\x1b") || ansi.StringWidth(colored) > width || !utf8.ValidString(colored) {
			t.Fatal(width, colored, plain, err, plainErr)
		}
		if ansi.Strip(colored) != plain {
			t.Fatal("colored and plain briefings differ", colored, plain)
		}
		if width == 250 && (!strings.Contains(colored, "\x1b[") || !strings.Contains(plain, "stdout: first last") || !strings.Contains(plain, "stderr: warning[31m")) {
			t.Fatal("missing highlight or labeled excerpts", colored, plain)
		}
	}
}

func TestChildBriefingPreservesEscapedActor(t *testing.T) {
	actor := "main/child_a-_x_-bcdefghijk"
	md := Tool("read", []byte(`{"path":"x"}`), []byte(`{"ok":true}`))
	text, err := TerminalBriefing(Inline(actor)+" · "+md.Summary, 120, false)
	if err != nil || !strings.Contains(text, actor) || strings.ContainsAny(text, "\n\r") {
		t.Fatal(text, err)
	}
}
