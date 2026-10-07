package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/llm"
	"ttc/internal/session"
)

func TestQuestionMouseWheelPreservesManualScroll(t *testing.T) {
	prompt := "FIRST_PROMPT_LINE\n" + strings.Repeat("Long question body\n", 60)
	arguments, err := json.Marshal(map[string]any{"questions": []session.Question{{ID: "long", Prompt: prompt, Options: []session.Option{{ID: "yes", Label: "LAST_OPTION"}, {ID: "no", Label: "Second option"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{{Calls: []llm.ToolCall{{ID: "long", Name: "question", Arguments: arguments}}}}})
	u.typeText("ask long question")
	u.key(tcell.KeyEnter)
	frame := u.wait(t, "LAST_OPTION")
	if strings.Contains(frame, "FIRST_PROMPT_LINE") {
		t.Fatal("fixture does not require scrolling")
	}
	for range 30 {
		u.screen.PostEventWait(tcell.NewEventMouse(50, 15, tcell.WheelUp, 0))
	}
	u.wait(t, "FIRST_PROMPT_LINE")
	// Keyboard navigation must leave manual-scroll mode and reveal the option
	// again, even when the wheel has moved far above the focus.
	u.key(tcell.KeyDown)
	u.wait(t, "Second option")
	for range 30 {
		u.screen.PostEventWait(tcell.NewEventMouse(50, 15, tcell.WheelUp, 0))
	}
	u.wait(t, "FIRST_PROMPT_LINE")
	for range 30 {
		u.screen.PostEventWait(tcell.NewEventMouse(50, 15, tcell.WheelDown, 0))
	}
	u.wait(t, "LAST_OPTION")
}
