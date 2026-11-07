package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/llm"
)

func TestGlobalChordsPrecedeFocusedComponents(t *testing.T) {
	for _, focus := range []string{"composer", "options", "editing", "submit", "window", "search", "picker", "preview"} {
		for _, advanced := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/advanced=%v", focus, advanced), func(t *testing.T) {
				c := pasteTestController()
				d := newQuestionDialog(questionFixture())
				d.answers[0].custom.set("saved answer")
				c.draft.set("saved composer")
				switch focus {
				case "options", "editing", "submit":
					c.question = d
					if focus == "editing" {
						d.answers[0].useCustom, d.answers[0].editing = true, true
					} else if focus == "submit" {
						d.tab = len(d.answers)
					}
				case "window":
					c.question = d
					c.modal.window = &Window{Text: "inspector"}
				case "search":
					c.modal.prompts = newPromptSearch([]string{"saved prompt"})
					c.modal.window = &c.modal.prompts.Window
				case "picker":
					c.modal.commands = newCommandMenu()
					c.modal.window = &c.modal.commands.Window
				case "preview":
					c.modal.preview = &imagePreview{}
				}
				before := append([]questionAnswer(nil), d.answers...)
				tab := d.tab
				prefix := tcell.NewEventKey(tcell.KeyCtrlX, "", 0)
				if advanced {
					prefix = tcell.NewEventKey(tcell.KeyRune, "x", tcell.ModCtrl)
				}
				c.handleScreenEvent(prefix)
				if !c.viewChord {
					t.Fatal("focused component swallowed global prefix")
				}
				c.handleScreenEvent(pasteRune("f"))
				if !c.fullscreen || !c.modal.empty() || c.viewChord || c.focusedQuestion() != nil {
					t.Fatal("global fullscreen shortcut did not take focus")
				}
				c.handleScreenEvent(tcell.NewEventKey(tcell.KeyEscape, "", 0))
				if c.fullscreen || !reflect.DeepEqual(before, d.answers) || d.tab != tab || c.draft.text != "saved composer" {
					t.Fatal("fullscreen round trip changed an input draft")
				}
				if focus == "options" || focus == "editing" || focus == "submit" || focus == "window" {
					if c.focusedQuestion() != d {
						t.Fatal("fullscreen exit did not restore inline question focus")
					}
				}
			})
		}
	}
}

func TestSidebarOverlayKeysPrecedeQuestion(t *testing.T) {
	c := pasteTestController()
	c.question = newQuestionDialog(questionFixture())
	c.handleKey(tcell.NewEventKey(tcell.KeyCtrlX, "", 0))
	c.handleKey(pasteRune("s"))
	if !c.sidebar.overlay {
		t.Fatal("question swallowed sidebar shortcut")
	}
	c.handleKey(tcell.NewEventKey(tcell.KeyTab, "", 0))
	if c.sidebar.focus != 1 || c.question.tab != 0 {
		t.Fatal("Tab did not belong exclusively to sidebar overlay")
	}
	c.handleKey(tcell.NewEventKey(tcell.KeyEscape, "", 0))
	if c.sidebar.overlay || c.question == nil {
		t.Fatal("closing sidebar dismissed question")
	}
}

func TestModalKeysAndPasteCannotEditBackgroundQuestionOrComposer(t *testing.T) {
	c := pasteTestController()
	c.question = newQuestionDialog(questionFixture())
	c.draft.set("composer")
	c.modal.window = &Window{Text: "Inspector"}
	c.screen = newTestScreen(t)
	if err := c.screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer c.screen.Fini()
	c.screen.SetSize(100, 30)
	c.handleKey(pasteRune("hidden edit"))
	c.handlePaste(tcell.NewEventPaste(true))
	c.handleKey(pasteRune("hidden paste"))
	c.handlePaste(tcell.NewEventPaste(false))
	if c.draft.text != "composer" || c.question.answers[0].custom.text != "" || c.modal.window == nil {
		t.Fatal("dialog input leaked to a background component")
	}
	c.handleKey(tcell.NewEventKey(tcell.KeyEscape, "", 0))
	if c.focusedQuestion() == nil {
		t.Fatal("closing inspector did not restore question focus")
	}
}

func TestPasteDoesNotActivateGlobalChords(t *testing.T) {
	c := pasteTestController()
	c.handlePaste(tcell.NewEventPaste(true))
	c.handleKey(tcell.NewEventKey(tcell.KeyCtrlX, "", 0))
	c.handleKey(pasteRune("f"))
	c.handlePaste(tcell.NewEventPaste(false))
	if c.fullscreen || c.viewChord || c.draft.text != "f" {
		t.Fatal("paste activated an application shortcut")
	}
}

func TestEscapeCancelsGlobalPrefixWithoutDismissingQuestion(t *testing.T) {
	c := pasteTestController()
	c.question = newQuestionDialog(questionFixture())
	c.handleKey(tcell.NewEventKey(tcell.KeyCtrlX, "", 0))
	c.handleKey(tcell.NewEventKey(tcell.KeyEscape, "", 0))
	if c.question == nil || c.viewChord {
		t.Fatal("Escape dismissed the question instead of canceling the prefix")
	}
}

func TestTranscriptScrollbarWheelWithEitherInput(t *testing.T) {
	for _, question := range []bool{false, true} {
		t.Run(fmt.Sprintf("question=%v", question), func(t *testing.T) {
			c := pasteTestController()
			c.screen = newTestScreen(t)
			if err := c.screen.Init(); err != nil {
				t.Fatal(err)
			}
			defer c.screen.Fini()
			c.screen.SetSize(80, 24)
			width := c.sidebar.bounds(80, 24, false)
			if question {
				c.question = newQuestionDialog(questionFixture())
			}
			for i := range 80 {
				c.view.append(line{text: fmt.Sprintf("ROW_%03d", i)})
			}
			c.view.viewport(width-1, 10)
			before := c.view.firstLine
			c.handleMouse(tcell.NewEventMouse(width-1, 1, tcell.WheelUp, 0))
			c.view.viewport(width-1, 10)
			if c.view.followTail || c.view.firstLine >= before {
				t.Fatal("wheel over the transcript scrollbar did not scroll its pane")
			}
		})
	}
}

func TestQuestionAllowsSidebarInspectionAndFullscreenRoundTrip(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: questionScript()})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEnter)
	u.wait(t, "Add notes?")
	u.typeText("preserved notes")
	u.wait(t, "Text: preserved notes")
	// The workspace is always present; live agent/job rows use this same route.
	u.screen.EventQ() <- tcell.NewEventMouse(90, 1, tcell.Button1, 0)
	u.wait(t, "Session & workspace")
	u.key(tcell.KeyEscape)
	u.wait(t, "Text: preserved notes")
	u.key(tcell.KeyCtrlX)
	u.typeText("f")
	frame := u.wait(t, "Copy mode · updates paused")
	if strings.Contains(frame, "Text: preserved notes") || u.screen.mouseEnabled.Load() {
		t.Fatal("fullscreen did not hide question and disable mouse reporting")
	}
	u.key(tcell.KeyEscape)
	u.wait(t, "Text: preserved notes")
	u.key(tcell.KeyEnter)
	u.wait(t, "Submit answers")
	u.key(tcell.KeyEnter)
	u.wait(t, "Done answering.")
	u.wait(t, "Turn complete")
	if u.runtime.PendingQuestion() != nil {
		t.Fatal("question could not submit after focus changes")
	}
}
