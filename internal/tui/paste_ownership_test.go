package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/session"
)

func TestPasteBatchBufferedTextCannotReachReplacementModal(t *testing.T) {
	for _, kind := range []string{"question", "search"} {
		t.Run(kind, func(t *testing.T) {
			c := pasteTestController()
			question := pasteQuestionFixture()
			search := newPromptSearch([]string{"pending"})
			if kind == "question" {
				c.modal.question, c.modal.window = question, &question.Window
			} else {
				c.modal.prompts, c.modal.window = search, &search.Window
			}
			c.handlePaste(tcell.NewEventPaste(true))
			c.batchingEvents = true
			c.handleKey(pasteRune("pending"))
			c.modal.clear()
			replacement := pasteQuestionFixture()
			c.modal.question, c.modal.window = replacement, &replacement.Window
			c.flushPaste()
			c.handleKey(pasteRune("remainder"))
			c.handlePaste(tcell.NewEventPaste(false))
			if question.answers[0].custom.text != "" || search.query.text != "" || replacement.answers[0].custom.text != "" || c.draft.text != "" {
				t.Fatal("buffered or subsequent paste escaped its vanished destination")
			}
		})
	}
}

func TestPasteBatchQuestionAnswerOwnershipSurvivesTabChange(t *testing.T) {
	c := pasteTestController()
	d := newQuestionDialog(session.QuestionForm{ID: "tabs", Questions: []session.Question{{ID: "first", Prompt: "First?"}, {ID: "second", Prompt: "Second?"}}})
	c.modal.question, c.modal.window = d, &d.Window
	c.handlePaste(tcell.NewEventPaste(true))
	c.batchingEvents = true
	c.handleKey(pasteRune("before"))
	d.tab = 1 // A mouse event can change the visible tab during a paste.
	c.handleKey(pasteRune("after"))
	c.handlePaste(tcell.NewEventPaste(false))
	if d.answers[0].custom.text != "beforeafter" || d.answers[1].custom.text != "" || d.tab != 1 {
		t.Fatal("paste changed answer ownership with the visible tab", d.answers, d.tab)
	}
}

func TestPasteBatchSearchFlushUpdatesFilterAndWindowTogether(t *testing.T) {
	c := pasteTestController()
	m := newPromptSearch([]string{"alpha needle", "beta other"})
	c.modal.prompts, c.modal.window = m, &m.Window
	c.handlePaste(tcell.NewEventPaste(true))
	c.batchingEvents = true
	c.handleKey(pasteRune("needle"))
	if m.query.text != "" {
		t.Fatal("query was edited per event")
	}
	c.flushPaste()
	if m.query.text != "needle" || len(m.matches) != 1 || m.entries[m.matches[0]] != "alpha needle" || !strings.Contains(m.Window.Header, "Search: needle▏") || !strings.Contains(m.Window.Text, "alpha") || strings.Contains(m.Window.Text, "beta") {
		t.Fatal("flush left stale search source or visible results", m.query.text, m.matches, m.Window.Header, m.Window.Text)
	}
}
