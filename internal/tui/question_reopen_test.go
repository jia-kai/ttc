package tui

import (
	"runtime"
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/llm"
	"ttc/internal/session"
)

func TestQuestionReopenAcknowledgmentCannotSwallowExit(t *testing.T) {
	c := newAsyncTestController(t, &llm.Script{Responses: questionScript()}, func(f *Frontend) {
		f.Plain = false
		f.Screen = newTestScreen(t)
	})
	c.submit(frontendSubmission{text: "Ask", ready: true})
	for c.modal.question == nil {
		nextControllerUpdate(t, c)
	}
	c.handleKey(tcell.NewEventKey(tcell.KeyEscape, "", 0))
	waitControllerResult(t, c, session.RequestDismissQuestion)
	c.openQuestions("")

	// The coordinator has reopened the form, but its acknowledgment has not
	// reached the UI. Escape and Ctrl+X are newer presentation intent.
	var reopened session.Update
	deadline := time.After(3 * time.Second)
	for reopened.Result == nil {
		select {
		case update := <-c.updates:
			if update.Result != nil && update.Result.Kind == session.RequestReopenQuestion {
				reopened = update
			} else if err := c.handleUpdate(update); err != nil {
				t.Fatal(err)
			}
		case <-deadline:
			t.Fatal("question reopen was not acknowledged")
		}
	}
	if reopened.Result.Err != nil {
		t.Fatal(reopened.Result.Err)
	}
	c.handleKey(tcell.NewEventKey(tcell.KeyEscape, "", 0))
	c.handleKey(tcell.NewEventKey(tcell.KeyCtrlX, "", 0))
	if err := c.handleUpdate(reopened); err != nil {
		t.Fatal(err)
	}
	next := c.handleKey(tcell.NewEventKey(tcell.KeyRune, "q", 0))
	if !next.ready || next.text != "/quit" {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("late reopen swallowed exit: submission=%+v modal=%v pending=%+v\n%s", next, c.modal.question != nil, c.frontend.Runtime.PendingQuestion(), buf[:n])
	}
	waitControllerResult(t, c, session.RequestDismissQuestion)
	if form := c.frontend.Runtime.PendingQuestion(); form == nil || !form.Dismissed || c.modal.question != nil {
		t.Fatal("Escape did not keep the reopened form dismissed", form)
	}
	if !c.submit(next) {
		t.Fatal("exit did not stop the frontend")
	}
	c.stopForeground()
	if c.frontend.Runtime.PendingQuestion() != nil {
		t.Fatal("exit did not join the active question")
	}
}
