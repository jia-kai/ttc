package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"ttc/internal/historytest"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/llm"
	"ttc/internal/session"
)

func TestComposerFrontendEditingRecallAndModelWindow(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{{Text: "Edited accepted."}}})
	u.typeText("alpha obsolete omega")
	u.screen.EventQ() <- tcell.NewEventKey(tcell.KeyRune, "b", tcell.ModAlt)
	u.key(tcell.KeyCtrlW)
	u.typeText("βeta ")
	u.screen.EventQ() <- tcell.NewEventKey(tcell.KeyRune, "f", tcell.ModAlt)
	u.wait(t, "> alpha βeta omega")
	u.key(tcell.KeyCtrlX)
	u.typeText("m")
	u.wait(t, "Model family")
	u.key(tcell.KeyEscape)
	u.key(tcell.KeyCtrlA)
	u.typeText("Hi ")
	u.wait(t, "> Hi alpha βeta omega")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	assertComposerSubmission(t, u, "Hi alpha βeta omega")
	u.typeText("unfinished")
	u.key(tcell.KeyLeft)
	u.key(tcell.KeyUp)
	u.wait(t, "> Hi alpha βeta omega")
	u.key(tcell.KeyDown)
	u.typeText("!")
	u.wait(t, "> unfinished!") // Recall restores the draft with its cursor at the end.
	u.key(tcell.KeyCtrlD)
	u.key(tcell.KeyCtrlU)
	u.wait(t, "> unfinished!")
}

func TestComposerFrontendPasteInMiddleDoesNotSubmit(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{{Text: "Paste accepted."}}})
	u.typeText("prefix suffix")
	u.key(tcell.KeyHome)
	for range 7 {
		u.key(tcell.KeyRight)
	}
	u.screen.EventQ() <- tcell.NewEventPaste(true)
	u.typeText("A")
	u.key(tcell.KeyEnter)
	u.key(tcell.KeyTab)
	u.typeText("B")
	u.screen.EventQ() <- tcell.NewEventPaste(false)
	waitComposerRows(t, u, "> prefix A", "  ⇥Bsuffix")
	var requests int
	if err := historytest.DB(t, u.runtime.Store).QueryRow("SELECT count(*) FROM model_requests").Scan(&requests); err != nil || requests != 0 {
		t.Fatal("paste triggered a request", requests, err)
	}
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	assertComposerSubmission(t, u, "prefix A\n\tBsuffix")
}

func TestComposerShiftEnterInsertsNewlineWithoutSubmitting(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{{Text: "Multiline accepted."}}})
	u.typeText("first")
	u.key(tcell.KeyCtrlJ)
	u.key(tcell.KeyCtrlK)
	u.wait(t, "> first")
	u.screen.EventQ() <- tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModShift)
	u.typeText("second")
	waitComposerRows(t, u, "> first", "  second")
	var requests int
	if err := historytest.DB(t, u.runtime.Store).QueryRow("SELECT count(*) FROM model_requests").Scan(&requests); err != nil || requests != 0 {
		t.Fatal("newline submitted the draft", requests, err)
	}
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	assertComposerSubmission(t, u, "first\nsecond")
	u.typeText("unfinished")
	u.key(tcell.KeyUp)
	waitComposerRows(t, u, "> first", "  second")
	u.key(tcell.KeyDown)
	u.typeText("!")
	u.wait(t, "> unfinished!")
}

func TestComposerPasteResetsModelChord(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{})
	u.key(tcell.KeyCtrlX)
	u.screen.EventQ() <- tcell.NewEventPaste(true)
	u.typeText("x")
	u.screen.EventQ() <- tcell.NewEventPaste(false)
	u.typeText("m")
	u.wait(t, "> xm")
}

func TestComposerFrontendScrollPreservesDraftAndMovementRevealsCursor(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{{Text: "Scrolled draft accepted."}}})
	for i, text := range []string{"zero", "one", "two", "three", "four", "five", "six", "seven"} {
		if i > 0 {
			u.screen.EventQ() <- tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModShift)
		}
		u.typeText(text)
	}
	waitComposerRows(t, u, "> two", "  three", "  four", "  five", "  six", "  seven")
	u.key(tcell.KeyCtrlK)
	u.key(tcell.KeyCtrlK)
	waitComposerRows(t, u, "> zero", "  one", "  two", "  three", "  four", "  five")
	u.key(tcell.KeyCtrlJ)
	waitComposerRows(t, u, "> one", "  two", "  three", "  four", "  five", "  six")
	u.key(tcell.KeyLeft)
	u.typeText("!")
	waitComposerRows(t, u, "> two", "  three", "  four", "  five", "  six", "  seve!n")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	assertComposerSubmission(t, u, "zero\none\ntwo\nthree\nfour\nfive\nsix\nseve!n")
}

func gatedComposerUI(t *testing.T, responses []llm.ScriptResponse) (*questionTestUI, *questionTestProvider) {
	t.Helper()
	p := &questionTestProvider{Script: llm.Script{Responses: responses}, ready: make(chan struct{}), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("run")
	u.key(tcell.KeyEnter)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("foreground request never started")
	}
	return u, p
}

func TestComposerPasteResetsInterruptPrefix(t *testing.T) {
	u, p := gatedComposerUI(t, []llm.ScriptResponse{{Text: "Not interrupted."}})
	u.key(tcell.KeyEscape)
	u.screen.EventQ() <- tcell.NewEventPaste(true)
	u.typeText("draft")
	u.screen.EventQ() <- tcell.NewEventPaste(false)
	u.key(tcell.KeyEscape)
	u.wait(t, "> draft")
	close(p.release)
	u.wait(t, "Turn complete")
}

func TestComposerPasteDefersQuestionOpening(t *testing.T) {
	u, p := gatedComposerUI(t, questionScript())
	u.screen.EventQ() <- tcell.NewEventPaste(true)
	u.typeText("A")
	u.wait(t, "> A")
	close(p.release)
	u.wait(t, "Waiting for answer")
	u.key(tcell.KeyEnter)
	u.key(tcell.KeyTab)
	u.typeText("B")
	waitComposerRows(t, u, "> A", "  ⇥B")
	u.screen.EventQ() <- tcell.NewEventPaste(false)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEscape)
	waitComposerRows(t, u, "> A", "  ⇥B")
	if u.runtime.PendingQuestion() == nil {
		t.Fatal("paste answered pending question")
	}
}

func TestComposerPasteDoesNotReopenDismissedQuestion(t *testing.T) {
	u, p := gatedComposerUI(t, questionScript())
	close(p.release)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEscape)
	u.typeText("kept")
	u.wait(t, "> kept")
	u.screen.EventQ() <- tcell.NewEventPaste(true)
	u.typeText("x")
	u.screen.EventQ() <- tcell.NewEventPaste(false)
	u.typeText("!")
	u.wait(t, "> keptx!")
}

func TestQuestionPasteRemainderDoesNotEnterComposerAfterClosure(t *testing.T) {
	u, p := gatedComposerUI(t, questionScript())
	u.typeText("untouched")
	u.wait(t, "> untouched")
	close(p.release)
	u.wait(t, "Choose a method?")
	u.screen.EventQ() <- tcell.NewEventPaste(true)
	u.typeText("A")
	// Wait for the pasted text so closure cannot race ahead of the UI capturing
	// the question destination.
	u.wait(t, "Text: A")
	form := u.runtime.PendingQuestion()
	if form == nil {
		t.Fatal("question not pending")
	}
	if err := u.runtime.AnswerQuestion(form.ID, []session.Answer{
		{ID: "choice", Values: []string{"a"}, Source: "option"},
		{ID: "notes", Values: []string{"confirmed"}, Source: "custom"},
	}); err != nil {
		t.Fatal(err)
	}
	u.wait(t, "Turn complete")
	u.typeText("discarded remainder")
	u.screen.EventQ() <- tcell.NewEventPaste(false)
	u.typeText("!")
	u.wait(t, "> untouched!")
}

func assertComposerSubmission(t *testing.T, u *questionTestUI, want string) {
	t.Helper()
	entries, err := u.runtime.Store.Branch(u.runtime.Current(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	for _, entry := range entries {
		if entry.Kind == "message" && entry.Role == "user" {
			var message llm.Message
			if err := json.Unmarshal(entry.Content, &message); err != nil {
				t.Fatal(err)
			}
			messages = append(messages, message.Content)
		}
	}
	if len(messages) != 1 || messages[0] != want {
		t.Fatal("submitted text", messages)
	}
}

// Match adjacent screen rows, ignoring only terminal right-padding.
func waitComposerRows(t *testing.T, u *questionTestUI, rows ...string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	var frame string
	for {
		select {
		case frame = <-u.screen.frames:
			lines := strings.Split(frame, "\n")
			for i := range lines {
				lines[i] = strings.TrimRight(lines[i], " ")
			}
			if strings.Contains(strings.Join(lines, "\n"), strings.Join(rows, "\n")+"\n") {
				return
			}
		case <-deadline:
			t.Fatalf("missing adjacent composer rows %q: %s", rows, frame)
		}
	}
}

func TestComposerDrawFollowsCursorAndCombiningCharacters(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(20, 10)
	c := newComposer("é界x")
	c.cursor = 2
	if err := draw(s, newTranscript(), newSidebar(), false, nil, nil, 0, c, 0, nil, nil, "", nil, llm.Selection{}); err != nil {
		t.Fatal(err)
	}
	x, y, visible := s.cursorX, s.cursorY, s.cursorVisible
	r, _, _ := s.Get(2, 8)
	if !visible || x != 3 || y != 8 || r != "é" {
		t.Fatal(x, y, visible, r)
	}
	s.SetSize(6, 10)
	c.set("abcdef")
	if err := draw(s, newTranscript(), newSidebar(), false, nil, nil, 0, c, 0, nil, nil, "", nil, llm.Selection{}); err != nil {
		t.Fatal(err)
	}
	x, y, visible = s.cursorX, s.cursorY, s.cursorVisible
	r, _, _ = s.Get(2, 8)
	if !visible || x != 4 || y != 8 || r != "e" {
		t.Fatal("narrow input viewport", x, y, visible, r)
	}
	rows := strings.Split(screenText(s), "\n")
	if strings.TrimRight(rows[7], " ") != "> abcd" || strings.TrimRight(rows[8], " ") != "  ef" {
		t.Fatal("wrapped input rows", rows[7:9])
	}
	c.cursor = 2
	if err := draw(s, newTranscript(), newSidebar(), false, nil, nil, 0, c, 0, nil, nil, "", nil, llm.Selection{}); err != nil {
		t.Fatal(err)
	}
	if !s.cursorVisible || s.cursorX != 4 || s.cursorY != 7 {
		t.Fatal("cursor before wrap", s.cursorX, s.cursorY, s.cursorVisible)
	}
	s.SetSize(10, 10)
	if err := draw(s, newTranscript(), newSidebar(), false, nil, nil, 0, c, 0, nil, nil, "", nil, llm.Selection{}); err != nil {
		t.Fatal(err)
	}
	if !s.cursorVisible || s.cursorX != 4 || s.cursorY != 8 {
		t.Fatal("cursor after resize", s.cursorX, s.cursorY, s.cursorVisible)
	}
	if got := strings.TrimSpace(strings.Split(screenText(s), "\n")[8]); got != "> abcdef" {
		t.Fatal("unwrapped row after resize", got)
	}
	c.set("0\n1\n2\n3\n4\n5\n6\n7")
	c.manualScroll, c.scrollTop = true, 0
	before := c.cursor
	if err := draw(s, newTranscript(), newSidebar(), false, nil, nil, 0, c, 0, nil, nil, "", nil, llm.Selection{}); err != nil {
		t.Fatal(err)
	}
	if s.cursorVisible || c.cursor != before {
		t.Fatal("manual scrolling did not hide the stationary cursor", s.cursorVisible, c.cursor)
	}
	c.key(tcell.NewEventKey(tcell.KeyLeft, "", 0))
	if err := draw(s, newTranscript(), newSidebar(), false, nil, nil, 0, c, 0, nil, nil, "", nil, llm.Selection{}); err != nil {
		t.Fatal(err)
	}
	if !s.cursorVisible || s.cursorX != 2 || s.cursorY != 8 {
		t.Fatal("movement did not reveal cursor after scrolling", s.cursorX, s.cursorY, s.cursorVisible)
	}
}
