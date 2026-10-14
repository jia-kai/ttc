package tui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/llm"
)

func TestFrontendCtrlCClearsPromptThenExits(t *testing.T) {
	for _, report := range []struct {
		name  string
		event *tcell.EventKey
	}{
		{"legacy", tcell.NewEventKey(tcell.KeyCtrlC, "", 0)},
		{"advanced", tcell.NewEventKeyEx(tcell.KeyRune, "c", tcell.ModCtrl, true, 0, 1)},
	} {
		for _, text := range []string{"draft α", " \t\n", "first\nsecond", "/he"} {
			t.Run(report.name+"/"+text, func(t *testing.T) {
				u := newQuestionTestUI(t, &llm.Script{})
				u.typeText(text)
				u.screen.EventQ() <- report.event
				// Typing at the reset cursor proves the entire draft was cleared
				// and that Ctrl-C did not exit or insert a literal control letter.
				u.typeText("fresh")
				u.wait(t, "> fresh")
				u.screen.EventQ() <- report.event
				u.screen.EventQ() <- report.event
				select {
				case err := <-u.done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("Ctrl-C did not exit with an empty prompt")
				}
			})
		}
	}
}

func TestFrontendCtrlCClearsRecalledPromptAndSavedDraft(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{{Text: "Seed complete."}}})
	u.typeText("seed")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	u.typeText("unfinished draft")
	u.key(tcell.KeyUp)
	u.wait(t, "> seed")
	u.key(tcell.KeyCtrlC)
	u.key(tcell.KeyUp)
	u.wait(t, "> seed")
	u.key(tcell.KeyDown)
	u.typeText("fresh")
	u.wait(t, "> fresh")
}

func TestFrontendCtrlCClearsPromptWithoutInterruptingTurn(t *testing.T) {
	p := &questionTestProvider{Script: llm.Script{Responses: questionScript()}, ready: make(chan struct{}), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not start")
	}
	u.typeText("unfinished draft")
	u.key(tcell.KeyCtrlC)
	u.typeText("fresh")
	u.wait(t, "> fresh")
	close(p.release)
	u.wait(t, "Second (Recommended)")
	// The global shortcut also clears the main prompt behind a question view,
	// without dismissing or cancelling the pending question.
	u.key(tcell.KeyCtrlC)
	u.key(tcell.KeyEnter)
	u.wait(t, "Add notes?")
	u.typeText("answer")
	u.key(tcell.KeyEnter)
	u.wait(t, "Submit answers")
	u.key(tcell.KeyEnter)
	u.wait(t, "Done answering.")
	u.wait(t, "Turn complete")
	u.typeText("after")
	u.wait(t, "> after")
}
