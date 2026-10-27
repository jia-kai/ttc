package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"
	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

func TestQuestionContextScrollPreservesAnswers(t *testing.T) {
	for _, mode := range []string{"options", "editing", "submit"} {
		for _, advanced := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/advanced=%v", mode, advanced), func(t *testing.T) {
				c := pasteTestController()
				d := newQuestionDialog(questionFixture())
				if mode == "editing" {
					d.answers[0].useCustom, d.answers[0].editing = true, true
					d.answers[0].custom.set("preserved draft")
					d.answers[0].custom.cursor = 3
				} else if mode == "submit" {
					d.tab = len(d.answers)
				}
				d.update()
				c.modal.question, c.modal.window = d, &d.Window
				for i := range 80 {
					c.view.append(line{text: fmt.Sprintf("CONTEXT_%03d", i)})
				}
				c.view.viewport(60, 10)
				initialLine := c.view.firstLine
				before := append([]questionAnswer(nil), d.answers...)
				tab, formScroll := d.tab, d.Window.Scroll
				key := func(letter string, legacy tcell.Key) {
					ev := tcell.NewEventKey(legacy, "", 0)
					if advanced {
						ev = tcell.NewEventKey(tcell.KeyRune, letter, tcell.ModCtrl)
					}
					if next := c.handleKey(ev); next.ready {
						t.Fatal("context scrolling submitted input")
					}
					c.view.viewport(60, 10)
				}
				key("u", tcell.KeyCtrlU)
				if c.view.followTail || c.view.firstLine >= initialLine {
					t.Fatal("Ctrl+U did not scroll conversation up")
				}
				key("d", tcell.KeyCtrlD)
				key("d", tcell.KeyCtrlD) // At bottom, Ctrl+D resumes following.
				if !c.view.followTail {
					t.Fatal("Ctrl+D did not return to the conversation tail")
				}
				if !reflect.DeepEqual(before, d.answers) || d.tab != tab || d.Window.Scroll != formScroll || c.modal.question != d {
					t.Fatal("context scrolling changed question state")
				}
			})
		}
	}
}

func TestQuestionWheelTargetsPaneUnderPointer(t *testing.T) {
	c := pasteTestController()
	s := newTestScreen(t)
	c.screen = s
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(100, 30)
	d := questionRenderDraft("draft", 2)
	d.form.Questions[0].Prompt = "First prompt\n" + strings.Repeat("Long prompt\n", 60)
	d.update()
	c.modal.question, c.modal.window = d, &d.Window
	for i := range 80 {
		c.view.append(line{text: fmt.Sprintf("ROW_%03d", i)})
	}
	g := c.layoutQuestionInput()
	c.view.viewport(g.bodyWidth, g.transcriptHeight)
	d.reveal(g.bodyWidth, g.bodyHeight)
	formScroll := d.Window.Scroll
	c.handleMouse(tcell.NewEventMouse(5, 1, tcell.WheelUp, 0))
	c.view.viewport(g.bodyWidth, g.transcriptHeight)
	if c.view.followTail || d.Window.Scroll != formScroll || d.manualScroll {
		t.Fatal("wheel over context scrolled the question instead")
	}
	contextScroll := c.view.firstLine
	c.handleMouse(tcell.NewEventMouse(5, g.bodyY, tcell.WheelUp, 0))
	c.view.viewport(g.bodyWidth, g.transcriptHeight)
	if d.Window.Scroll >= formScroll || !d.manualScroll || c.view.firstLine != contextScroll {
		t.Fatal("wheel over question did not preserve context position")
	}
}

func TestQuestionReopenLeavesCopyMode(t *testing.T) {
	c := pasteTestController()
	c.view.append(line{text: "conversation context"})
	c.setFullscreen(true)
	c.sidebar.overlay = true
	if c.copyView == nil {
		t.Fatal("fixture did not enter copy mode")
	}
	form := questionFixture()
	c.showQuestion(&form)
	if c.fullscreen || c.copyView != nil || c.displayView() != c.view || c.sidebar.overlay || c.modal.question == nil {
		t.Fatal("question opened over a frozen copy-mode transcript")
	}
}

func TestQuestionWheelSkipsSteeringStatusRows(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: questionScript()})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	if err := u.runtime.Steer(contextbuild.Input{Text: "retained instruction"}); err != nil {
		t.Fatal(err)
	}
	c := pasteTestController()
	c.frontend.Runtime = u.runtime
	s := newTestScreen(t)
	c.screen = s
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(100, 40)
	d := newQuestionDialog(questionFixture())
	c.modal.question, c.modal.window = d, &d.Window
	for i := range 80 {
		c.view.append(line{text: fmt.Sprintf("ROW_%03d", i)})
	}
	g := c.layoutQuestionInput()
	if g.queuedRows != 1 {
		t.Fatalf("steering preview missing from hit-test geometry: %+v", g)
	}
	c.view.viewport(g.bodyWidth, g.transcriptHeight)
	c.handleMouse(tcell.NewEventMouse(5, g.transcriptHeight, tcell.WheelUp, 0))
	if !c.view.followTail || d.manualScroll {
		t.Fatal("wheel over the status row scrolled a content pane")
	}
}
