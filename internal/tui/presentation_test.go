package tui

import (
	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
	"strings"
	"testing"
	"ttc/internal/llm"
)

func TestSharedPresentationInterleavingAndFinalImmutability(t *testing.T) {
	v := newTranscript()
	v.assistant(1, "assistant", "First ", 0, false)
	v.publish(line{text: "awaiting shell", callID: "pending:1", awaiting: true}, "", false)
	v.assistant(2, "assistant · child", "Other", 0, false)
	v.assistant(1, "assistant", "reply", 0, false)
	v.publish(line{text: "**shell** running", callID: "call1", brief: true, markdown: true}, "pending:1", false)
	v.assistant(1, "assistant", "# First reply", 41, true)
	v.publish(line{text: "**shell** done", callID: "call1", id: 42, complete: true, brief: true, markdown: true}, "", false)
	if v.assistant(1, "assistant", "late", 0, false) || v.assistant(1, "assistant", "duplicate", 41, true) {
		t.Fatal("accepted late assistant publication")
	}
	if v.publish(line{text: "late", callID: "call1"}, "", false) {
		t.Fatal("accepted late tool update")
	}
	if len(v.lines) != 3 || v.lines[0].text != "# First reply" || v.lines[2].text != "Other" || !v.lines[0].markdown {
		t.Fatal(v.lines)
	}
	if _, ok := v.calls["pending:1"]; ok {
		t.Fatal("pending alias survived")
	}
	rows := v.viewport(40, 30)
	label := 0
	for _, row := range rows {
		text := ansi.Strip(row.text)
		if text == "assistant" {
			label++
			continue
		}
		if strings.Contains(text, "First reply") && !strings.HasPrefix(text, "  ") {
			t.Fatal("unindented assistant body", text)
		}
	}
	if label != 1 {
		t.Fatal("assistant label count", label)
	}
	replay := newTranscript()
	replay.append(v.lines[0])
	if replay.assistant(1, "assistant", "duplicate", 41, true) || len(replay.lines) != 1 {
		t.Fatal("completion duplicated replay")
	}
}

func TestFrozenPresentationDoesNotChangeOnPublishOrResize(t *testing.T) {
	live := newTranscript()
	live.assistant(1, "assistant", "before", 0, false)
	frozen := live.snapshot()
	live.assistant(1, "assistant", "after", 1, true)
	live.append(line{text: "new tool", callID: "new"})
	frozen.viewport(20, 10)
	frozen.viewport(12, 10)
	if len(frozen.lines) != 1 || frozen.lines[0].text != "before" || frozen.lines[0].markdown {
		t.Fatal(frozen.lines)
	}
	if len(live.lines) != 2 || live.lines[0].text != "after" {
		t.Fatal(live.lines)
	}
}

func TestStreamingSpeakerDrawUsesStyledCells(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(60, 12)
	v := newTranscript()
	v.assistant(1, "assistant", "plain *streamed* body", 0, false)
	if err := draw(s, v, newSidebar(), false, nil, nil, -1, newComposer(""), 0, nil, nil, "", nil, llm.Selection{}); err != nil {
		t.Fatal(err)
	}
	for i, r := range "assistant" {
		got, _, _, _ := s.GetContent(i, 0)
		if got != r {
			t.Fatal("speaker ANSI leaked", i, got, r)
		}
	}
	body := ""
	for x := range 50 {
		r, _, _, _ := s.GetContent(x, 1)
		body += string(r)
	}
	if !strings.HasPrefix(body, "  plain *streamed* body") {
		t.Fatal(body)
	}
}
