package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/history"
	"scicode/internal/provider"
	"scicode/internal/session"
)

func TestPromptSearchNewestFirstEditingAndExactSelection(t *testing.T) {
	long := "ALPHA\n" + strings.Repeat("界", 5000)
	m := newPromptSearch([]string{"old alpha", "βeta", long, "new alpha"})
	for _, r := range "AlPhA" {
		m.key(tcell.NewEventKey(tcell.KeyRune, r, 0), 10)
	}
	if !reflect.DeepEqual(m.matches, []int{3, 2, 0}) || !utf8.ValidString(m.Window.Text) || strings.Contains(m.Window.Text, "βeta") {
		t.Fatal(m.matches, m.Window.Text)
	}
	m.key(tcell.NewEventKey(tcell.KeyCtrlR, 0, 0), 10)
	if text, closed := m.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0), 10); !closed || text != long {
		t.Fatal("selection was truncated")
	}
	m.key(tcell.NewEventKey(tcell.KeyCtrlU, 0, 0), 10)
	m.key(tcell.NewEventKey(tcell.KeyRune, 'β', 0), 10)
	if len(m.matches) != 1 || m.entries[m.matches[0]] != "βeta" {
		t.Fatal(m.matches)
	}
	m.key(tcell.NewEventKey(tcell.KeyBackspace2, 0, 0), 10)
	if len(m.matches) != 4 {
		t.Fatal("editing failed to refilter")
	}
	m.query.pasting = true
	for range 400 {
		m.key(tcell.NewEventKey(tcell.KeyRune, 'x', 0), 10)
	}
	m.query.pasting = false
	if utf8.RuneCountInString(m.query.text) != 256 {
		t.Fatal("unbounded query")
	}
	if text, closed := m.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0), 10); text != "" || closed {
		t.Fatal("empty match accepted")
	}
	if text, closed := m.key(tcell.NewEventKey(tcell.KeyEscape, 0, 0), 10); text != "" || !closed {
		t.Fatal("cancel failed")
	}
}

func TestPromptSearchBoundedRowsAndWrappedMouse(t *testing.T) {
	entries := make([]string, history.MaxPromptHistoryEntries)
	for i := range entries {
		entries[i] = fmt.Sprintf("prompt %d %s", i, strings.Repeat("long ", 1000))
	}
	m := newPromptSearch(entries)
	for range 100 {
		m.key(tcell.NewEventKey(tcell.KeyPgDn, 0, 0), 20)
		m.reveal(40, 12)
	}
	if len(m.rows) > historyWindowRows || len(m.Window.Text) > historyWindowRows*600 || m.selected != len(entries)-1 {
		t.Fatal("unbounded render", len(m.rows), len(m.Window.Text), m.selected)
	}
	m = newPromptSearch([]string{"older wrapped entry", "newer wrapped entry"})
	left, top, width, height := windowBounds(24, 24)
	inner := width - 2
	headers := len(m.Window.HeaderLines(inner, height-3))
	row := len(wrap(m.rows[0], inner))
	m.mouse(tcell.NewEventMouse(left+1, top+headers+row+1, tcell.Button1, 0), 24, 24)
	if m.selected != 1 {
		t.Fatal("mouse selected wrong prompt", m.selected)
	}
}

func TestPromptRecallAndSearchSavedSessionInNewFrontend(t *testing.T) {
	u := newQuestionTestUIWithSetup(t, &provider.Script{}, nil, func(r *session.Runtime) {
		id := history.NewID("session")
		turn, _, err := r.Store.StartSession(id, r.Workspace.Root, r.CurrentSelection(), provider.Message{Role: "user", Content: "older saved α prompt"})
		if err != nil {
			t.Fatal(err)
		}
		if err = r.Store.FinishTurn(turn, "completed"); err != nil {
			t.Fatal(err)
		}
		id = history.NewID("session")
		turn, _, err = r.Store.StartSession(id, r.Workspace.Root, r.CurrentSelection(), provider.Message{Role: "user", Content: "newer saved β prompt"})
		if err != nil {
			t.Fatal(err)
		}
		if err = r.Store.FinishTurn(turn, "completed"); err != nil {
			t.Fatal(err)
		}
	})
	u.typeText("unfinished draft")
	u.key(tcell.KeyUp)
	u.wait(t, "> newer saved β prompt")
	u.key(tcell.KeyUp)
	u.wait(t, "> older saved α prompt")
	u.key(tcell.KeyDown)
	u.key(tcell.KeyDown)
	u.wait(t, "> unfinished draft")
	u.key(tcell.KeyCtrlR)
	u.wait(t, "Prompt history search")
	u.typeText("OLDER")
	u.wait(t, "Matches 1")
	u.key(tcell.KeyEscape)
	u.wait(t, "> unfinished draft")
	u.key(tcell.KeyCtrlR)
	u.typeText("older")
	u.wait(t, "Matches 1")
	u.key(tcell.KeyEnter)
	u.wait(t, "> older saved α prompt")
	var requests, sessions int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM model_requests").Scan(&requests); err != nil || requests != 0 {
		t.Fatal("recall submitted a prompt", requests, err)
	}
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&sessions); err != nil || sessions != 2 {
		t.Fatal("recall persisted an empty session", sessions, err)
	}
}

// Supply a private JSON string array through TTC_PROMPT_BENCH_DATA to benchmark
// real session prompts. Normal benchmark runs use a self-contained bounded fixture.
func BenchmarkPromptSearch(b *testing.B) {
	var h promptHistory
	if path := os.Getenv("TTC_PROMPT_BENCH_DATA"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			b.Fatal(err)
		}
		var entries []string
		if err = json.Unmarshal(data, &entries); err != nil {
			b.Fatal(err)
		}
		for _, s := range entries {
			h.add(s)
		}
	} else {
		for i := 0; i < history.MaxPromptHistoryEntries; i++ {
			h.add(fmt.Sprintf("Research experiment %d: %s", i, strings.Repeat("measurement α beta ", 400)))
		}
	}
	m := newPromptSearch(h.entries)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.query.set([]string{"research", "experiment", "no-matching-token", "α"}[i%4])
		m.filter()
	}
	b.ReportMetric(float64(len(h.entries)), "prompts")
	b.ReportMetric(float64(h.bytes), "text-bytes")
}

func TestPromptHistoryStartupCancellation(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := Frontend{Runtime: u.runtime}
	if err := f.Run(ctx); err == nil {
		t.Fatal("canceled startup succeeded")
	}
}

func TestRecalledCommandsAndPathsKeepHistoryNavigation(t *testing.T) {
	u := newQuestionTestUIWithSetup(t, &provider.Script{}, nil, func(r *session.Runtime) {
		for _, text := range []string{"old prompt", "@README.md", "/help"} {
			turn, _, err := r.Store.StartSession(history.NewID("session"), r.Workspace.Root, r.CurrentSelection(), provider.Message{Role: "user", Content: text})
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Store.FinishTurn(turn, "completed"); err != nil {
				t.Fatal(err)
			}
		}
	})
	u.typeText("draft")
	u.key(tcell.KeyUp)
	u.wait(t, "> /help")
	u.key(tcell.KeyUp)
	u.wait(t, "> @README.md")
	u.key(tcell.KeyUp)
	u.wait(t, "> old prompt")
	u.key(tcell.KeyDown)
	u.wait(t, "> @README.md")
	u.key(tcell.KeyDown)
	u.wait(t, "> /help")
	u.key(tcell.KeyDown)
	u.wait(t, "> draft")
}
