package tui

import (
	"testing"
	"ttc/internal/historytest"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/llm"
)

func TestBlankSessionPickerDoesNotCreateWorkspaceOrSession(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{})
	u.typeText("Unsent research draft")
	u.key(tcell.KeyCtrlX)
	u.typeText("l")
	u.wait(t, "No sessions in this workspace")
	u.key(tcell.KeyEscape)
	u.wait(t, "Unsent research draft")
	for _, table := range []string{"workspaces", "sessions", "entries", "turns"} {
		var count int
		if err := historytest.DB(t, u.runtime.Store).QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
}
