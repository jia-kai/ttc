package tui

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/llm"
	"ttc/internal/session"
)

func newPickerFocusController(t *testing.T, configure func(*Frontend)) *frontendController {
	t.Helper()
	return newAsyncTestController(t, &llm.Script{Responses: questionScript()}, func(f *Frontend) {
		f.Plain = false
		f.Screen = newTestScreen(t)
		if configure != nil {
			configure(f)
		}
	})
}

func pickerFocusQuestion(t *testing.T, c *frontendController) *questionDialog {
	t.Helper()
	c.submit(frontendSubmission{text: "Ask", ready: true})
	for c.question == nil {
		nextControllerUpdate(t, c)
	}
	d := c.question
	d.tab = 1
	d.answers[1].custom.set("preserved answer")
	d.answers[1].custom.cursor = 3
	d.answers[1].custom.killed = "preserved kill buffer"
	d.update()
	return d
}

func openComposerPicker(t *testing.T, c *frontendController, kind, value string) {
	t.Helper()
	if kind == "history" {
		c.recall.add(value)
		c.handleKey(tcell.NewEventKey(tcell.KeyCtrlR, "", 0))
		return
	}
	c.handleKey(tcell.NewEventKey(tcell.KeyCtrlP, "", 0))
	for i, command := range slashCommands {
		if command.value == value {
			c.modal.commands.selected = i
			return
		}
	}
	t.Fatal("command missing from palette", value)
}

func pickerAnswerState(d *questionDialog) []questionAnswer {
	answers := append([]questionAnswer(nil), d.answers...)
	for i := range answers {
		answers[i].custom.layout = nil // Drawing may refresh this immutable cache.
	}
	return answers
}

func TestPickerFocusCtrlROlderPreservesQuery(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		c := newPickerFocusController(t, nil)
		c.recall.add("older matching prompt")
		c.recall.add("newer matching prompt")
		key := tcell.NewEventKey(tcell.KeyCtrlR, "", 0)
		if advanced {
			key = tcell.NewEventKey(tcell.KeyRune, "r", tcell.ModCtrl)
		}
		c.handleKey(key)
		search := c.modal.prompts
		c.handleKey(pasteRune("matching"))
		c.handleKey(key)
		if c.modal.prompts != search || search.query.text != "matching" || search.selected != 1 {
			t.Fatalf("Ctrl-R reset search: replaced=%v query=%q selected=%d", c.modal.prompts != search, search.query.text, search.selected)
		}
		c.handleKey(tcell.NewEventKey(tcell.KeyEnter, "", 0))
		if c.draft.text != "older matching prompt" {
			t.Fatal("Ctrl-R did not recall the older matching prompt", c.draft.text)
		}
	}
}

func TestPickerFocusAcceptedResultDismissesQuestionAndReopensDraft(t *testing.T) {
	for _, kind := range []string{"commands", "history"} {
		t.Run(kind, func(t *testing.T) {
			c := newPickerFocusController(t, nil)
			d := pickerFocusQuestion(t, c)
			before := pickerAnswerState(d)
			c.draft.set("previous composer")
			openComposerPicker(t, c, kind, "/jobs")
			c.handleKey(tcell.NewEventKey(tcell.KeyEnter, "", 0))
			if c.question != nil || c.retainedQuestion != d || c.draft.text != "/jobs" || !c.modal.empty() {
				t.Fatal("accepted picker did not reveal the selected composer draft")
			}
			if _, err := c.prepare(); err != nil {
				t.Fatal(err)
			}
			if c.question != nil || d.tab != 1 || !reflect.DeepEqual(before, pickerAnswerState(d)) {
				t.Fatal("picker changed retained question state or immediately reopened it")
			}
			result := waitControllerResult(t, c, session.RequestDismissQuestion)
			if result.Err != nil {
				t.Fatal(result.Err)
			}
			if form := c.frontend.Runtime.PendingQuestion(); form == nil || !form.Dismissed {
				t.Fatal("picker did not dismiss the runtime question", form)
			}
			// Palette and history still fill rather than execute. A second Enter
			// runs the local command without completing the pending question.
			c.submit(c.handleKey(tcell.NewEventKey(tcell.KeyEnter, "", 0)))
			if form := c.frontend.Runtime.PendingQuestion(); form == nil || !form.Dismissed || c.modal.window == nil {
				t.Fatal("local command completed the question or did not execute", form)
			}
			c.openQuestions("")
			result = waitControllerResult(t, c, session.RequestReopenQuestion)
			if result.Err != nil || c.question != d || d.tab != 1 || !reflect.DeepEqual(before, pickerAnswerState(d)) || c.composerQuestionID != "" {
				t.Fatal("explicit reopening lost retained state or composer focus fence", result.Err)
			}
		})
	}
}

func TestPickerFocusCancellationPreservesQuestion(t *testing.T) {
	for _, kind := range []string{"commands", "history"} {
		t.Run(kind, func(t *testing.T) {
			c := newPickerFocusController(t, nil)
			d := pickerFocusQuestion(t, c)
			before := pickerAnswerState(d)
			c.draft.set("previous composer")
			openComposerPicker(t, c, kind, "/jobs")
			c.handleKey(tcell.NewEventKey(tcell.KeyEscape, "", 0))
			form := c.frontend.Runtime.PendingQuestion()
			if c.focusedQuestion() != d || form == nil || form.Dismissed || c.draft.text != "previous composer" || d.tab != 1 || !reflect.DeepEqual(before, pickerAnswerState(d)) || c.composerQuestionID != "" {
				t.Fatal("canceling picker changed question, composer or runtime dismissal", form)
			}
		})
	}
}

func TestPickerFocusAcceptanceFencesDeferredQuestionPublication(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	c := newPickerFocusController(t, func(f *Frontend) {
		f.Reconnect = func() error {
			close(entered)
			<-release
			return nil
		}
	})
	// Hold the initial publication behind a picker, without creating inline
	// state. Slow unrelated controls must not delay mailbox question controls.
	c.handleKey(tcell.NewEventKey(tcell.KeyCtrlP, "", 0))
	c.submit(frontendSubmission{text: "Ask", ready: true})
	for c.deferredQuestionID == "" {
		nextControllerUpdate(t, c)
	}
	form := c.frontend.Runtime.PendingQuestion()
	c.control(session.Request{Kind: session.RequestReconnect})
	defer close(release)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("control worker did not block")
	}
	openComposerPicker(t, c, "commands", "/jobs")
	c.handleKey(tcell.NewEventKey(tcell.KeyEnter, "", 0))
	if c.question != nil || c.deferredQuestionID != "" || c.questionReopenRequest != 0 || c.composerQuestionID != form.ID {
		t.Fatal("acceptance retained prior presentation intent")
	}
	// Deliver an already-queued publication without first consuming dismissal's
	// acknowledgment. Acceptance must have canceled its presentation intent.
	if err := c.handleRuntimeEvent(session.Event{Kind: "question", Generation: c.frontend.Runtime.Generation(), Question: form}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.prepare(); err != nil {
		t.Fatal(err)
	}
	if c.question != nil || c.deferredQuestionID != "" || c.draft.text != "/jobs" {
		t.Fatal("queued publication stole accepted composer focus")
	}
	if result := waitControllerResult(t, c, session.RequestDismissQuestion); result.Err != nil {
		t.Fatal(result.Err)
	}
	if pending := c.frontend.Runtime.PendingQuestion(); pending == nil || !pending.Dismissed {
		t.Fatal("slow effect worker delayed mailbox dismissal", pending)
	}
}

func TestPickerFocusAcceptanceSupersedesOlderReopenAcknowledgment(t *testing.T) {
	c := newPickerFocusController(t, nil)
	d := pickerFocusQuestion(t, c)
	if err := c.frontend.Runtime.DismissQuestion(d.form.ID); err != nil {
		t.Fatal(err)
	}
	c.syncQuestionState(c.frontend.Runtime.PendingQuestion())
	c.openQuestions("")
	// Capture an older acknowledgment without delivering it to the controller.
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
			t.Fatal("reopen was not acknowledged")
		}
	}
	openComposerPicker(t, c, "history", "recalled main prompt")
	c.handleKey(tcell.NewEventKey(tcell.KeyEnter, "", 0))
	if err := c.handleUpdate(reopened); err != nil {
		t.Fatal(err)
	}
	if _, err := c.prepare(); err != nil {
		t.Fatal(err)
	}
	if c.question != nil || c.deferredQuestionID != "" || c.questionReopenRequest != 0 || c.draft.text != "recalled main prompt" {
		t.Fatal("older reopen acknowledgment stole accepted composer focus")
	}
	waitControllerResult(t, c, session.RequestDismissQuestion)
	// A genuinely newer explicit reopen must still work.
	c.openQuestions("")
	waitControllerResult(t, c, session.RequestReopenQuestion)
	if c.question != d || c.composerQuestionID != "" {
		t.Fatal("composer acceptance blocked a newer explicit reopen")
	}
}

func TestPickerFocusRecalledInputRedirectsDismissedQuestion(t *testing.T) {
	for _, mode := range []string{"immediate", "blocked reconnect", "queued dismiss and reopen"} {
		t.Run(mode, func(t *testing.T) {
			responses := questionScript()
			responses[1] = llm.ScriptResponse{Prefix: "user: Recalled redirect", Text: "Redirect received."}
			entered, release := make(chan struct{}), make(chan struct{})
			c := newAsyncTestController(t, &llm.Script{Responses: responses}, func(f *Frontend) {
				f.Plain = false
				f.Screen = newTestScreen(t)
				f.Reconnect = func() error {
					close(entered)
					<-release
					return nil
				}
			})
			defer close(release)
			d := pickerFocusQuestion(t, c)
			if mode != "immediate" {
				c.control(session.Request{Kind: session.RequestReconnect})
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("reconnect did not block")
				}
			}
			if mode == "queued dismiss and reopen" {
				// All question controls must share the input mailbox's order.
				// An older reopen cannot run later on the slow effect worker and
				// undo the dismissal caused by the accepted history result.
				c.control(session.Request{Kind: session.RequestDismissQuestion, ID: d.form.ID})
				c.openQuestions("")
			}
			openComposerPicker(t, c, "history", "Recalled redirect")
			c.handleKey(tcell.NewEventKey(tcell.KeyEnter, "", 0))
			// The user can immediately press Enter again. Do not consume any
			// dismissal acknowledgment or release the blocked effect worker.
			c.submit(c.handleKey(tcell.NewEventKey(tcell.KeyEnter, "", 0)))
			if result := waitControllerResult(t, c, session.RequestInput); result.Err != nil {
				t.Fatal(result.Err)
			}
			if form := c.frontend.Runtime.PendingQuestion(); form != nil {
				t.Fatal("immediate recalled input failed to redirect the question", form)
			}
			waitControllerState(t, c, func(state session.CoordinatorState) bool { return !state.Busy })
			messages, err := c.frontend.Runtime.Store.Messages(c.frontend.Runtime.Current())
			if err != nil {
				t.Fatal(err)
			}
			resultAt, inputAt, redirects := -1, -1, 0
			for i, message := range messages {
				if message.Type == llm.TypeToolResult && message.CallID == "q" {
					var result struct {
						Dismissed bool            `json:"dismissed"`
						Answers   json.RawMessage `json:"answers"`
					}
					if err := json.Unmarshal([]byte(message.Content), &result); err != nil || !result.Dismissed || len(result.Answers) != 0 {
						t.Fatal("wrong question result", message.Content, err)
					}
					resultAt = i
				}
				if message.Type == llm.TypeUserInput && message.Content == "Recalled redirect" {
					inputAt, redirects = i, redirects+1
				}
			}
			if resultAt < 0 || inputAt <= resultAt || redirects != 1 {
				t.Fatal("recalled redirect was lost, duplicated or out of order", messages)
			}
		})
	}
}
