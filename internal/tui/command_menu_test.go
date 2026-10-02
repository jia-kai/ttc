package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/provider"
)

func TestCommandMenuNarrowRevealAndCancel(t *testing.T) {
	m := newCommandMenu()
	m.key(tcell.NewEventKey(tcell.KeyEnd, 0, 0), 3)
	m.reveal(12, 3)
	if m.Window.Scroll <= 0 {
		t.Fatal("last action stayed outside the narrow viewport")
	}
	if command, closed := m.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0), 3); !closed || command != "/quit" {
		t.Fatal(command, closed)
	}
	if command, closed := m.key(tcell.NewEventKey(tcell.KeyEscape, 0, 0), 3); !closed || command != "" {
		t.Fatal(command, closed)
	}
}

func TestCommandPalettePreservesDraftUntilSelection(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{})
	u.typeText("Retained input")
	u.key(tcell.KeyCtrlP)
	u.wait(t, "Enter fills input")
	u.key(tcell.KeyEscape)
	u.wait(t, "> Retained input")
	u.key(tcell.KeyCtrlP)
	u.key(tcell.KeyEnd)
	u.key(tcell.KeyEnter)
	u.wait(t, "> /quit")
	var count int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM turns").Scan(&count); err != nil || count != 0 {
		t.Fatal("palette submitted input", count, err)
	}
	u.key(tcell.KeyEnter)
	select {
	case err := <-u.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("selected exit action did not work")
	}
}

func TestLeaderLiveInspectorsDuringActiveTurn(t *testing.T) {
	p := &questionTestProvider{Script: provider.Script{Responses: []provider.ScriptResponse{{Text: "Settled"}}}, ready: make(chan struct{}), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("Start")
	u.key(tcell.KeyEnter)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not start")
	}
	u.typeText("Retained draft")
	for _, action := range []string{"j", "t"} {
		u.key(tcell.KeyCtrlX)
		u.typeText(action)
		frame := u.wait(t, map[string]string{"j": "┌jobs · Esc closes", "t": "┌timers · Esc closes"}[action])
		if strings.Contains(frame, "requires an idle turn") {
			t.Fatal(frame)
		}
		u.key(tcell.KeyEscape)
		u.wait(t, "> Retained draft")
	}
	close(p.release)
	u.wait(t, "Settled")
}

func TestLeaderNewQuestionAndExit(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{Responses: questionScript()})
	oldID := u.runtime.Current()
	u.typeText("Retained draft")
	u.key(tcell.KeyCtrlX)
	u.typeText("n")
	deadline := time.Now().Add(3 * time.Second)
	for u.runtime.Current() == oldID {
		if time.Now().After(deadline) {
			t.Fatal("new-session shortcut did not run")
		}
		time.Sleep(time.Millisecond)
	}
	u.wait(t, "> Retained draft")
	u.key(tcell.KeyCtrlA)
	u.key(tcell.KeyCtrlK)
	u.typeText("Ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEscape)
	u.key(tcell.KeyCtrlX)
	u.typeText("?")
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEscape)
	u.key(tcell.KeyCtrlX)
	u.typeText("q")
	select {
	case err := <-u.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("exit shortcut did not join active question")
	}
}

func TestEditorCommandUsesTerminalEditor(t *testing.T) {
	u := newQuestionTestUIWithEditor(t, &provider.Script{}, func(ctx context.Context, text string) (string, error) {
		return "Edited through command", nil
	})
	u.typeText("/editor")
	u.key(tcell.KeyEnter)
	u.wait(t, "> Edited through command")
}
