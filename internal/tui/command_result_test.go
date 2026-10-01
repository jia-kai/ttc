package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/provider"
)

func assertNoCommandWindow(t *testing.T, frame string) {
	t.Helper()
	if strings.Contains(frame, "Command result") {
		t.Fatal("action confirmation opened a window")
	}
}

func TestUndoRedoConfirmInConversationAndLoadErrorsRemainVisible(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{Responses: []provider.ScriptResponse{
		{Calls: []provider.ToolCall{{ID: "write", Name: "write", Arguments: []byte(`{"path":"result.txt","content":"saved"}`)}}},
		{Text: "Saved the result."},
	}})
	u.typeText("write a result")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn completed")
	for _, command := range []string{"undo", "redo"} {
		u.typeText("/" + command)
		u.key(tcell.KeyEnter)
		assertNoCommandWindow(t, u.wait(t, command+" completed"))
	}
	current := u.runtime.Current()
	u.typeText("/load nonexistent-session")
	u.key(tcell.KeyEnter)
	u.wait(t, "Error:")
	if u.runtime.Current() != current {
		t.Fatal("failed load replaced the session")
	}
}
