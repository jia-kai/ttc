package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/provider"
)

func TestFullscreenCopiesWithoutMouseAndRestoresEveryExit(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{Responses: []provider.ScriptResponse{{Text: "Copy target"}, {Text: "Submitted"}}})
	u.typeText("hello")
	u.key(tcell.KeyEnter)
	u.wait(t, "Copy target")
	if !u.screen.mouseEnabled.Load() {
		t.Fatal("normal mouse disabled")
	}
	waitMode := func(full bool) string {
		t.Helper()
		timeout := time.After(3 * time.Second)
		for {
			select {
			case frame := <-u.screen.frames:
				if u.screen.mouseEnabled.Load() == !full && strings.Contains(frame, "WORKSPACE") == !full {
					return frame
				}
			case <-timeout:
				t.Fatal("fullscreen did not change", full)
			}
		}
	}
	toggle := func() { u.key(tcell.KeyCtrlX); u.typeText("f") }
	for _, exit := range []string{"toggle", "escape", "submit"} {
		toggle()
		frame := waitMode(true)
		if strings.ContainsRune(frame, '█') {
			t.Fatal("fullscreen has scrollbar")
		}
		// A mouse report queued before disabling must not open a conversation modal.
		u.screen.PostEventWait(tcell.NewEventMouse(3, 1, tcell.Button1, 0))
		u.key(tcell.KeyCtrlU)
		frame = waitMode(true)
		if strings.Contains(frame, "╭") {
			t.Fatal("fullscreen mouse opened inspector")
		}
		switch exit {
		case "toggle":
			toggle()
		case "escape":
			u.key(tcell.KeyEscape)
		case "submit":
			u.typeText("next")
			u.key(tcell.KeyEnter)
		}
		waitMode(false)
	}
	u.wait(t, "Submitted")
}

func TestFullscreenConversationUsesLastColumnWithoutScrollbar(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(20, 12)
	view := newTranscript()
	for i := range 50 {
		view.append(line{text: strings.Repeat("x", 20), id: int64(i + 1)})
	}
	sidebar := newSidebar()
	if err := draw(screen, view, sidebar, true, nil, nil, -1, newComposer(""), 0, nil, nil, "", nil, provider.Selection{}); err != nil {
		t.Fatal(err)
	}
	for y := range 11 {
		r, _, _, _ := screen.GetContent(19, y)
		if r != 'x' {
			t.Fatal("fullscreen column reserved for scrollbar", y, r)
		}
	}
	for x := range 20 {
		r, _, _, _ := screen.GetContent(x, 11)
		if r == '%' || r == '~' {
			t.Fatal("fullscreen scroll indicator retained")
		}
	}
}

func TestFullscreenQuestionStillWorksWithKeyboard(t *testing.T) {
	p := &questionTestProvider{Script: provider.Script{Responses: questionScript()}, ready: make(chan struct{}), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	<-p.ready
	u.key(tcell.KeyCtrlX)
	u.typeText("f")
	deadline := time.After(3 * time.Second)
	for u.screen.mouseEnabled.Load() {
		select {
		case <-u.screen.frames:
		case <-deadline:
			t.Fatal("fullscreen did not disable mouse before question")
		}
	}
	close(p.release)
	// Background prompts stay deferred while selecting copy text.
	u.key(tcell.KeyEscape)
	u.wait(t, "Choose a method?")
	if !u.screen.mouseEnabled.Load() {
		t.Fatal("question did not restore normal mouse capture")
	}
	u.key(tcell.KeyEnter)
	u.wait(t, "Add notes?")
	u.typeText("Keep units.")
	u.key(tcell.KeyRight)
	u.wait(t, "Submit answers")
	u.key(tcell.KeyEnter)
	u.wait(t, "Done answering.")
	if !u.screen.mouseEnabled.Load() {
		t.Fatal("question exit changed normal mouse mode")
	}
}

func TestFullscreenPausesPeriodicRedrawAndLiveReply(t *testing.T) {
	p := &questionTestProvider{Script: provider.Script{Responses: []provider.ScriptResponse{{Text: "Reply produced while copying"}}}, ready: make(chan struct{}), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("hello")
	u.key(tcell.KeyEnter)
	<-p.ready
	u.key(tcell.KeyCtrlX)
	u.typeText("f")
	u.wait(t, "Copy mode · updates paused")
	for {
		select {
		case <-u.screen.frames:
		default:
			goto drained
		}
	}
drained:
	close(p.release)
	// Runtime events and two timer ticks must not redraw the selected screen.
	select {
	case frame := <-u.screen.frames:
		t.Fatal("copy mode redrew without user input", frame)
	case <-time.After(650 * time.Millisecond):
	}
	u.key(tcell.KeyEscape)
	u.wait(t, "Reply produced while copying")
}
