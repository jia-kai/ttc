package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/llm"
	"ttc/internal/session"
)

func TestQuestionDismissReopenDoesNotBlockOnFullEventSink(t *testing.T) {
	saturated, release := make(chan struct{}), make(chan struct{})
	sink := make(chan session.Event, 1)
	sink <- session.Event{Kind: "status", Text: "occupies the bounded sink"}
	u := newQuestionTestUIWithSetup(t, &llm.Script{Responses: questionScript()}, nil, func(r *session.Runtime) {
		emit := r.Emit
		r.Emit = func(e session.Event) {
			select {
			case <-saturated:
				select {
				case sink <- e:
				case <-release:
				}
			default:
				if emit != nil {
					emit(e)
				}
			}
		}
	})
	// Unblock any unexpected publication before the frontend's cleanup runs,
	// so this test also fails cleanly against a synchronous emitting setter.
	t.Cleanup(func() { close(release) })
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	close(saturated)
	u.key(tcell.KeyEscape)
	u.wait(t, "What would you like to do instead?")
	if form := u.runtime.PendingQuestion(); form == nil || !form.Dismissed {
		t.Fatal("dismissal did not update runtime state", form)
	}
	u.typeText("/questions")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	if form := u.runtime.PendingQuestion(); form == nil || form.Dismissed {
		t.Fatal("reopen did not update runtime state", form)
	}
}

func TestDelayedInitialQuestionEventDoesNotReopenDismissal(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: questionScript()})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	initial := u.runtime.PendingQuestion()
	if initial == nil || initial.Dismissed {
		t.Fatal("initial question was not live", initial)
	}
	u.key(tcell.KeyEnd)
	u.key(tcell.KeyEnter)
	u.typeText("retained answer")
	u.key(tcell.KeyEscape) // Leave text editing before dismissing the form.
	u.key(tcell.KeyEscape)
	u.wait(t, "What would you like to do instead?")
	// Replay the original immutable snapshot after the dismissal control has
	// settled. It must not override the newer live question state.
	u.runtime.Publish(session.Event{Kind: "question", Question: initial})
	// Both events use the same ordered sink; drawing this marker proves that
	// the delayed initial event has been handled without reopening the form.
	u.runtime.Publish(session.Event{Kind: "status", Text: "◆◆ delayed question processed", Generation: u.runtime.Generation()})
	frame := u.wait(t, "◆◆")
	if form := u.runtime.PendingQuestion(); form == nil || !form.Dismissed {
		t.Fatal("delayed initial event reopened a user dismissal", form, frame)
	}
	if strings.Contains(frame, "Choose a method?") || strings.Contains(frame, "Waiting for answer") {
		t.Fatal("delayed initial event restored stale presentation", frame)
	}
	u.typeText("/questions")
	u.key(tcell.KeyEnter)
	u.wait(t, "Text: retained answer")
}

func TestClosedQuestionHasNoLiveStatus(t *testing.T) {
	for _, dismissed := range []bool{false, true} {
		name := "answered"
		if dismissed {
			name = "redirected"
		}
		t.Run(name, func(t *testing.T) {
			u := newQuestionTestUI(t, &llm.Script{Responses: questionScript()})
			u.typeText("ask")
			u.key(tcell.KeyEnter)
			u.wait(t, "Choose a method?")
			form := u.runtime.PendingQuestion()
			if form == nil {
				t.Fatal("question not pending")
			}
			if dismissed {
				u.key(tcell.KeyEscape)
				u.wait(t, "What would you like to do instead?")
				u.typeText("change task")
				u.key(tcell.KeyEnter)
			} else if err := u.runtime.AnswerQuestion(form.ID, []session.Answer{
				{ID: "choice", Values: []string{"a"}, Source: "option"},
				{ID: "notes", Values: []string{"done"}, Source: "custom"},
			}); err != nil {
				t.Fatal(err)
			}
			frame := u.wait(t, "Turn complete")
			if form := u.runtime.PendingQuestion(); form != nil {
				t.Fatal(form)
			}
			if strings.Contains(frame, "What would you like to do instead?") || strings.Contains(frame, "Waiting for answer") {
				t.Fatalf("resolved form still rendered as live status: %s", frame)
			}
			if _, err := u.runtime.Store.Entry(form.EntryID); err != nil {
				t.Fatal("closing status lost inspectable tool intent", err)
			}
		})
	}
}
