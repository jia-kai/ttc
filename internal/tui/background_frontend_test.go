package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/llm"
)

func TestBackgroundMenuSelectsForegroundShell(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{
		{Calls: []llm.ToolCall{{ID: "shell", Name: "shell", Arguments: []byte(`{"command":"touch started; while [ ! -e release ]; do sleep 0.01; done; printf done","wake_on_exit":false}`)}}},
		{Text: "Foreground released"},
	}})
	u.typeText("start")
	u.key(tcell.KeyEnter)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(u.runtime.Workspace.Root, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shell did not start")
		}
		time.Sleep(time.Millisecond)
	}
	u.typeText("/background")
	u.key(tcell.KeyEnter)
	u.wait(t, "Foreground shells")
	u.key(tcell.KeyEnter)
	u.wait(t, "Foreground released")
	u.wait(t, "Turn complete")
	if jobs := u.runtime.Jobs.Live(); len(jobs) != 1 || jobs[0].Kind != "shell" {
		t.Fatal("promotion canceled command", jobs)
	}
	u.typeText("draft")
	u.key(tcell.KeyCtrlB)
	u.typeText("x")
	u.wait(t, "> drafxt")
	if err := os.WriteFile(filepath.Join(u.runtime.Workspace.Root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCtrlBPromotesForegroundShellWithoutSubmittingDraft(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{
		{Calls: []llm.ToolCall{{ID: "shell", Name: "shell", Arguments: []byte(`{"command":"touch started; sleep 30","wake_on_exit":false}`)}}},
		{Text: "Shortcut released"},
	}})
	u.typeText("start")
	u.key(tcell.KeyEnter)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(u.runtime.Workspace.Root, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shell did not start")
		}
		time.Sleep(time.Millisecond)
	}
	u.typeText("retained")
	u.key(tcell.KeyCtrlB)
	u.wait(t, "Shortcut released")
	u.wait(t, "> retained")
}
