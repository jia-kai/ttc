package tui

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/provider"
	"scicode/internal/session"
)

func TestComposerFrontendEditingRecallAndModelWindow(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{Responses: []provider.ScriptResponse{{Text: "Edited accepted."}}})
	u.typeText("alpha obsolete omega")
	u.screen.PostEventWait(tcell.NewEventKey(tcell.KeyRune, 'b', tcell.ModAlt))
	u.key(tcell.KeyCtrlW)
	u.typeText("βeta ")
	u.screen.PostEventWait(tcell.NewEventKey(tcell.KeyRune, 'f', tcell.ModAlt))
	u.wait(t, "> alpha βeta omega")
	u.key(tcell.KeyCtrlX)
	u.typeText("m")
	u.wait(t, "Model family")
	u.key(tcell.KeyEscape)
	u.key(tcell.KeyCtrlA)
	u.typeText("Hi ")
	u.wait(t, "> Hi alpha βeta omega")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn completed")
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
	u := newQuestionTestUI(t, &provider.Script{Responses: []provider.ScriptResponse{{Text: "Paste accepted."}}})
	u.typeText("prefix suffix")
	u.key(tcell.KeyHome)
	for range 7 {
		u.key(tcell.KeyRight)
	}
	u.screen.PostEventWait(tcell.NewEventPaste(true))
	u.typeText("A")
	u.key(tcell.KeyEnter)
	u.key(tcell.KeyTab)
	u.typeText("B")
	u.screen.PostEventWait(tcell.NewEventPaste(false))
	u.wait(t, "> prefix A↵⇥Bsuffix")
	var requests int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM model_requests").Scan(&requests); err != nil || requests != 0 {
		t.Fatal("paste triggered a request", requests, err)
	}
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn completed")
	assertComposerSubmission(t, u, "prefix A\n\tBsuffix")
}

func TestComposerCtrlJInsertsNewlineWithoutSubmitting(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{Responses: []provider.ScriptResponse{{Text: "Multiline accepted."}}})
	u.typeText("first")
	u.key(tcell.KeyCtrlJ)
	u.typeText("second")
	u.wait(t, "> first↵second")
	var requests int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM model_requests").Scan(&requests); err != nil || requests != 0 {
		t.Fatal("newline submitted the draft", requests, err)
	}
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn completed")
	assertComposerSubmission(t, u, "first\nsecond")
}

func TestComposerPasteResetsModelChord(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{})
	u.key(tcell.KeyCtrlX)
	u.screen.PostEventWait(tcell.NewEventPaste(true))
	u.typeText("x")
	u.screen.PostEventWait(tcell.NewEventPaste(false))
	u.typeText("m")
	u.wait(t, "> xm")
}

func gatedComposerUI(t *testing.T, responses []provider.ScriptResponse) (*questionTestUI, *questionTestProvider) {
	t.Helper()
	p := &questionTestProvider{Script: provider.Script{Responses: responses}, ready: make(chan struct{}), release: make(chan struct{})}
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
	u, p := gatedComposerUI(t, []provider.ScriptResponse{{Text: "Not interrupted."}})
	u.key(tcell.KeyEscape)
	u.screen.PostEventWait(tcell.NewEventPaste(true))
	u.typeText("draft")
	u.screen.PostEventWait(tcell.NewEventPaste(false))
	u.key(tcell.KeyEscape)
	u.wait(t, "> draft")
	close(p.release)
	u.wait(t, "Turn completed")
}

func TestComposerPasteDefersQuestionOpening(t *testing.T) {
	u, p := gatedComposerUI(t, questionScript())
	u.screen.PostEventWait(tcell.NewEventPaste(true))
	u.typeText("A")
	u.wait(t, "> A")
	close(p.release)
	u.wait(t, "Waiting for answer")
	u.key(tcell.KeyEnter)
	u.key(tcell.KeyTab)
	u.typeText("B")
	u.wait(t, "> A↵⇥B")
	u.screen.PostEventWait(tcell.NewEventPaste(false))
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEscape)
	u.wait(t, "> A↵⇥B")
	if forms := u.runtime.PendingQuestions(); len(forms) != 1 {
		t.Fatal("paste answered pending question", forms)
	}
}

func TestComposerPasteDoesNotReopenDismissedQuestion(t *testing.T) {
	u, p := gatedComposerUI(t, questionScript())
	close(p.release)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEscape)
	u.typeText("kept")
	u.wait(t, "> kept")
	u.screen.PostEventWait(tcell.NewEventPaste(true))
	u.typeText("x")
	u.screen.PostEventWait(tcell.NewEventPaste(false))
	u.typeText("!")
	u.wait(t, "> keptx!")
}

func TestQuestionPasteRemainderDoesNotEnterComposerAfterClosure(t *testing.T) {
	u, p := gatedComposerUI(t, questionScript())
	u.typeText("untouched")
	u.wait(t, "> untouched")
	close(p.release)
	u.wait(t, "Choose a method?")
	u.screen.PostEventWait(tcell.NewEventPaste(true))
	u.typeText("A")
	u.wait(t, "Other")
	forms := u.runtime.PendingQuestions()
	if len(forms) != 1 {
		t.Fatal(forms)
	}
	if err := u.runtime.AnswerQuestion(forms[0].ID, []session.Answer{
		{ID: "choice", Values: []string{"a"}, Source: "option"},
		{ID: "notes", Values: []string{"confirmed"}, Source: "custom"},
	}); err != nil {
		t.Fatal(err)
	}
	u.wait(t, "Turn completed")
	u.typeText("discarded remainder")
	u.screen.PostEventWait(tcell.NewEventPaste(false))
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
			var message provider.Message
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

func TestComposerDrawFollowsCursorAndCombiningCharacters(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(20, 10)
	c := newComposer("é界x")
	c.cursor = 2
	if err := draw(s, newTranscript(), newSidebar(), false, nil, nil, 0, c, 0, nil, nil, "", nil, provider.Selection{}); err != nil {
		t.Fatal(err)
	}
	x, y, visible := s.GetCursor()
	r, marks, _, _ := s.GetContent(2, 8)
	if !visible || x != 3 || y != 8 || r != 'e' || string(marks) != "́" {
		t.Fatal(x, y, visible, r, marks)
	}
	s.SetSize(6, 10)
	c.set("abcdef")
	if err := draw(s, newTranscript(), newSidebar(), false, nil, nil, 0, c, 0, nil, nil, "", nil, provider.Selection{}); err != nil {
		t.Fatal(err)
	}
	x, y, visible = s.GetCursor()
	r, _, _, _ = s.GetContent(2, 8)
	if !visible || x != 5 || y != 8 || r != 'd' {
		t.Fatal("narrow input viewport", x, y, visible, r)
	}
}
