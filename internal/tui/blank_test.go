package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
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
		if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
}
