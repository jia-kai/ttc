package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
	"ttc/internal/session"
)

func TestInlineQuestionGlobalEditorPreservesModal(t *testing.T) {
	for _, inline := range []bool{false, true} {
		input := make(chan string, 1)
		c := newAsyncTestController(t, &llm.Script{Responses: questionScript()}, func(f *Frontend) {
			f.Plain = false
			f.Screen = newTestScreen(t)
			f.EditInput = func(_ context.Context, text string) (string, error) {
				input <- text
				return "edited input", nil
			}
		})
		c.draft.set("composer draft")
		if inline {
			c.submit(frontendSubmission{text: "Ask", ready: true})
			for c.question == nil {
				nextControllerUpdate(t, c)
			}
			c.question.tab = 1
			c.question.answers[1].custom.set("question draft")
			c.question.update()
		}
		inspector := &Window{Title: "Inspector"}
		c.modal.window = inspector
		c.startEditor()
		select {
		case result := <-c.editorDone:
			if err := c.handleEditorResult(result); err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("global editor did not finish while inspector was open")
		}
		want := "composer draft"
		if inline {
			want = "question draft"
			if c.question.answers[1].custom.text != "edited input" || c.draft.text != "composer draft" {
				t.Fatal("global editor did not update only the inline question")
			}
		} else if c.draft.text != "edited input" {
			t.Fatal("global editor did not update the composer")
		}
		if got := <-input; got != want || c.modal.window != inspector {
			t.Fatal("global editor used the wrong draft or discarded the inspector", got)
		}
	}
}

func TestInlineQuestionFullscreenEditorTargetsComposer(t *testing.T) {
	input := make(chan string, 1)
	c := newAsyncTestController(t, nil, func(f *Frontend) {
		f.Plain = false
		f.Screen = newTestScreen(t)
		f.EditInput = func(_ context.Context, text string) (string, error) {
			input <- text
			return "edited composer", nil
		}
	})
	c.draft.set("composer draft")
	c.question = questionRenderDraft("question draft", 3)
	c.setFullscreen(true)
	c.startEditor()
	select {
	case result := <-c.editorDone:
		if err := c.handleEditorResult(result); err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fullscreen composer editor did not finish")
	}
	if got := <-input; got != "composer draft" || c.draft.text != "edited composer" || c.question.answers[0].custom.text != "question draft" || !c.fullscreen {
		t.Fatal("fullscreen editor changed the hidden question or targeted it instead of the composer", got)
	}
}

func TestInlineQuestionStateSurvivesModalAndFullscreen(t *testing.T) {
	c := pasteTestController()
	form := questionFixture()
	c.showQuestion(&form)
	d := c.question
	d.tab = 1
	d.answers[1].custom.set("preserved\nanswer")
	d.answers[1].custom.cursor = 3
	d.update()
	before := append([]questionAnswer(nil), d.answers...)
	if !c.modal.empty() || c.modal.window != nil || c.focusedQuestion() != d {
		t.Fatal("inline form acquired modal ownership")
	}
	inspector := &Window{Title: "Inspector", Text: "Details"}
	c.modal.window = inspector
	if c.focusedQuestion() != nil || c.question != d {
		t.Fatal("inspector replaced or focused the background form")
	}
	c.modal.clear()
	if c.focusedQuestion() != d {
		t.Fatal("closing inspector did not restore inline focus")
	}
	c.setFullscreen(true)
	if c.focusedQuestion() != nil || c.question != d {
		t.Fatal("copy mode discarded or focused the hidden form")
	}
	c.setFullscreen(false)
	if c.focusedQuestion() != d || d.tab != 1 || !reflect.DeepEqual(d.answers, before) {
		t.Fatal("inspecting or copy mode changed question draft, cursor, or tab")
	}
}

func TestSyncInlineQuestionDoesNotCloseInspector(t *testing.T) {
	for _, state := range []string{"dismissed", "replaced", "closed"} {
		t.Run(state, func(t *testing.T) {
			c := pasteTestController()
			form := questionFixture()
			c.showQuestion(&form)
			retained := c.question
			retained.answers[0].custom.set("draft")
			inspector := &Window{Title: "Inspector"}
			c.modal.window = inspector
			c.viewChord = true
			pending := &form
			switch state {
			case "dismissed":
				form.Dismissed = true
			case "replaced":
				form.ID += "-next"
			case "closed":
				pending = nil
			}
			c.syncQuestionState(pending)
			if c.question != nil || c.modal.window != inspector || !c.viewChord {
				t.Fatal("question reconciliation left the form open or changed inspector input state")
			}
			if state == "dismissed" {
				if c.retainedQuestion != retained || retained.answers[0].custom.text != "draft" {
					t.Fatal("dismissal discarded the retained draft")
				}
				form.Dismissed = false
				c.showQuestion(&form)
				if c.question != retained || c.modal.window != inspector || c.focusedQuestion() != nil {
					t.Fatal("reopening replaced the inspector or discarded the retained form")
				}
			} else if c.retainedQuestion != nil {
				t.Fatal("closed or replaced question retained a stale draft")
			}
		})
	}
}

func TestInlineQuestionGenerationResetPreservesUnrelatedModal(t *testing.T) {
	c := pasteTestController()
	form := questionFixture()
	c.showQuestion(&form)
	inspector := &Window{Title: "Inspector"}
	c.modal.window = inspector
	c.queueGeneration = c.frontend.Runtime.Generation() + 1
	c.deferredQuestionID, c.questionReopenRequest = form.ID, 10
	if _, err := c.prepare(); err != nil {
		t.Fatal(err)
	}
	if c.question != nil || c.retainedQuestion != nil || c.deferredQuestionID != "" || c.questionReopenRequest != 0 || c.modal.window != inspector {
		t.Fatal("generation reset retained inline state or cleared an unrelated modal")
	}
}

func TestAsyncInlineQuestionDefersUntilInputAvailable(t *testing.T) {
	for _, blocker := range []string{"modal", "paste", "fullscreen", "editor"} {
		t.Run(blocker, func(t *testing.T) {
			c := newAsyncTestController(t, &llm.Script{Responses: questionScript()}, func(f *Frontend) {
				f.Plain = false
				f.Screen = newTestScreen(t)
			})
			inspector := &Window{Title: "Inspector"}
			switch blocker {
			case "modal":
				c.modal.window = inspector
			case "paste":
				c.draft.pasting = true
			case "fullscreen":
				c.setFullscreen(true)
			case "editor":
				c.editing = true
				t.Cleanup(func() { c.editing = false })
			}
			c.submit(frontendSubmission{text: "Ask", ready: true})
			for c.deferredQuestionID == "" {
				nextControllerUpdate(t, c)
			}
			if _, err := c.prepare(); err != nil {
				t.Fatal(err)
			}
			if c.question != nil || c.deferredQuestionID == "" || blocker == "modal" && c.modal.window != inspector {
				t.Fatal("asynchronous question stole focus before the blocker closed")
			}
			c.modal.clear()
			c.draft.pasting, c.editing = false, false
			c.setFullscreen(false)
			if _, err := c.prepare(); err != nil {
				t.Fatal(err)
			}
			if c.question == nil || c.focusedQuestion() != c.question || !c.modal.empty() {
				t.Fatal("deferred question did not open inline after focus became available")
			}
			question := c.question
			c.setFullscreen(true)
			if err := c.drawFrame(); err != nil {
				t.Fatal(err)
			}
			if c.question != question || strings.Contains(screenText(c.screen), question.form.Questions[0].Prompt) || !strings.Contains(screenText(c.screen), "Copy mode") {
				t.Fatal("fullscreen drew or discarded the inline question")
			}
			c.setFullscreen(false)
			if err := c.drawFrame(); err != nil {
				t.Fatal(err)
			}
			if c.question != question || !strings.Contains(screenText(c.screen), question.form.Questions[0].Prompt) {
				t.Fatal("leaving fullscreen did not restore the inline question")
			}
			c.draft.set("/he")
			if _, err := c.prepare(); err != nil {
				t.Fatal(err)
			}
			if c.draft.completion != nil {
				t.Fatal("inline question left composer completion active")
			}
			c.pendingBTW = []session.Event{{Generation: c.frontend.Runtime.Generation(), Text: "Aside answer"}}
			if _, err := c.prepare(); err != nil {
				t.Fatal(err)
			}
			if len(c.pendingBTW) != 1 || !c.modal.empty() || c.focusedQuestion() != question {
				t.Fatal("asynchronous aside stole inline question focus")
			}
		})
	}
}

func TestQuestionReopenAcknowledgmentDefersToInspector(t *testing.T) {
	c := newAsyncTestController(t, &llm.Script{Responses: questionScript()}, func(f *Frontend) {
		f.Plain = false
		f.Screen = newTestScreen(t)
	})
	c.submit(frontendSubmission{text: "Ask", ready: true})
	for c.question == nil {
		nextControllerUpdate(t, c)
	}
	form := c.frontend.Runtime.PendingQuestion()
	if err := c.frontend.Runtime.DismissQuestion(form.ID); err != nil {
		t.Fatal(err)
	}
	c.syncQuestionState(c.frontend.Runtime.PendingQuestion())
	c.modal.window = &Window{Title: "Previous inspector"}
	c.setFullscreen(true)
	c.openQuestions("")
	if !c.modal.empty() || c.fullscreen {
		t.Fatal("explicit reopen did not release previous modal/copy-mode focus")
	}
	// A newer interaction wins over the delayed reopen acknowledgment.
	inspector := &Window{Title: "Inspector"}
	c.modal.window = inspector
	waitControllerResult(t, c, session.RequestReopenQuestion)
	if c.question != nil || c.deferredQuestionID != form.ID || c.modal.window != inspector {
		t.Fatal("asynchronous reopen replaced the focused inspector")
	}
	c.modal.clear()
	if _, err := c.prepare(); err != nil {
		t.Fatal(err)
	}
	if c.question == nil || c.question.form.ID != form.ID {
		t.Fatal("deferred reopen did not restore the inline question")
	}
}

func TestInlineQuestionModalRendersAboveFormAndHidesCursor(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(80, 24)
	d := questionRenderDraft("retained answer", 3)
	inspector := &Window{Title: "Focused inspector", Text: "Independent modal body"}
	frame := drawState{view: newTranscript(), sidebar: newSidebar(), focused: -1, draft: newComposer("hidden"), question: d, window: inspector}
	if err := draw(s, frame); err != nil {
		t.Fatal(err)
	}
	if text := screenText(s); !strings.Contains(text, "Focused inspector") || !strings.Contains(text, "Independent modal body") || s.cursorVisible {
		t.Fatalf("modal did not overlay the question with its cursor hidden: cursor=%v\n%s", s.cursorVisible, text)
	}
	frame.window = nil
	if err := draw(s, frame); err != nil {
		t.Fatal(err)
	}
	if !s.cursorVisible || !strings.Contains(screenText(s), "Text: retained answer") {
		t.Fatal("closing modal did not restore the unchanged inline form and cursor")
	}
}
