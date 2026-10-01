package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/history"
	"scicode/internal/provider"
)

func TestSessionPickerReloadPreservesEditedDraft(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	u := newQuestionTestUIWithEditor(t, &provider.Script{Responses: []provider.ScriptResponse{{Text: "Original research reply"}}}, func(ctx context.Context, draft string) (string, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return draft, ctx.Err()
		}
		return "Retained composer draft", nil
	})
	first := u.runtime.Current()
	u.typeText("Start research")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn completed")
	if _, err := u.runtime.Store.DB.Exec("UPDATE sessions SET name='Original research' WHERE id=?", first); err != nil {
		t.Fatal(err)
	}
	u.typeText("/new")
	u.key(tcell.KeyEnter)
	u.wait(t, "New session ·")
	u.typeText("/sessions")
	u.key(tcell.KeyEnter)
	u.wait(t, "Enter reload")
	u.key(tcell.KeyCtrlX)
	u.typeText("e")
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("editor did not start from picker")
	}
	for len(u.screen.frames) > 0 {
		<-u.screen.frames
	}
	close(release)
	u.wait(t, "Enter reload") // First resumed frame; the modal covers the composer.
	u.key(tcell.KeyDown)
	u.key(tcell.KeyEnter)
	u.wait(t, "Retained composer draft")
	assertNoCommandWindow(t, u.wait(t, "Original research"))
	if u.runtime.Current() != first {
		t.Fatal("picker submitted draft instead of reloading")
	}
	var turns int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM turns").Scan(&turns); err != nil || turns != 1 {
		t.Fatal("draft was submitted", turns, err)
	}
}

func TestSessionMenuDateGroupingSelectionAndWrapping(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.FixedZone("local", 3600))
	days := []int{0, -1, -2, -6, -7}
	list := []history.Session{}
	for i, day := range days {
		list = append(list, history.Session{ID: string(rune('a' + i)), Name: strings.Repeat("Research ", 8), LastActivityMS: now.AddDate(0, 0, day).UnixMilli()})
	}
	m := newSessionMenu(list, "a", now)
	for _, want := range []string{"Today", "Yesterday", "Monday", "Thursday", "2026-09-23"} {
		if !strings.Contains(m.Window.Text, want) {
			t.Fatal(want, m.Window.Text)
		}
	}
	m.key(tcell.NewEventKey(tcell.KeyDown, 0, 0), 10)
	m.reveal(12, 5)
	if m.selected != 1 || m.Window.Scroll == 0 {
		t.Fatal(m.selected, m.Window.Scroll)
	}
	id, closed := m.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0), 10)
	if !closed || id != "b" {
		t.Fatal(id, closed)
	}
	empty := newSessionMenu(nil, "", now)
	if id, _ := empty.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0), 10); id != "" {
		t.Fatal(id)
	}
}
func TestSessionPickerLoadsSelectedSession(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{Responses: []provider.ScriptResponse{{Text: "First session evidence"}}})
	first := u.runtime.Current()
	u.typeText("hello")
	u.key(tcell.KeyEnter)
	u.wait(t, "First session evidence")
	u.wait(t, "Turn completed")
	if _, err := u.runtime.Store.DB.Exec("UPDATE sessions SET name='Original research' WHERE id=?", first); err != nil {
		t.Fatal(err)
	}
	u.typeText("/new")
	u.key(tcell.KeyEnter)
	u.wait(t, "New session ·")
	if u.runtime.Current() == first {
		t.Fatal("new did not switch")
	}
	u.key(tcell.KeyCtrlX)
	u.typeText("l")
	u.wait(t, "Enter reload")
	u.wait(t, "Today")
	u.key(tcell.KeyDown)
	u.key(tcell.KeyEnter)
	frame := u.wait(t, "First session evidence")
	if u.runtime.Current() != first {
		t.Fatal("picker did not reload selected session")
	}
	assertNoCommandWindow(t, frame)
}
