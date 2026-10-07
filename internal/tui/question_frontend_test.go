package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"ttc/internal/tool"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/graphics"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/scratch"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/workspace"
)

type questionTestProvider struct {
	llm.Script
	ready, release chan struct{}
	first          bool
}

func (p *questionTestProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	if !p.first {
		p.first = true
		close(p.ready)
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.Script.Stream(ctx, req, emit)
}

type questionTestUI struct {
	screen         *observedScreen
	runtime        *session.Runtime
	done           chan error
	questionEvents chan struct{}
	cancel         context.CancelFunc
}

func newQuestionTestUI(t *testing.T, p session.Inference, sinks ...*graphics.Kitty) *questionTestUI {
	t.Helper()
	return newQuestionTestUIWithEditor(t, p, nil, sinks...)
}

func newQuestionTestUIWithEditor(t *testing.T, p session.Inference, editor func(context.Context, string) (string, error), sinks ...*graphics.Kitty) *questionTestUI {
	return newQuestionTestUIWithSetup(t, p, editor, nil, sinks...)
}

func newQuestionTestUIWithSetup(t *testing.T, p session.Inference, editor func(context.Context, string) (string, error), setup func(*session.Runtime), sinks ...*graphics.Kitty) *questionTestUI {
	t.Helper()
	if _, err := scratch.Verify(); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	w, err := workspace.Open(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}
	selection := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
	catalog, err := skills.Discover(context.Background(), w.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan session.Event, 64)
	questionEvents := make(chan struct{}, 8)
	r, err := session.New(ctx, store, w, p, selection, "", catalog, tool.WebSearchConfig{}, func(e session.Event) {
		select {
		case events <- e:
			if e.Kind == "question" {
				questionEvents <- struct{}{}
			}
		case <-ctx.Done():
		}
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	r.AutoName = false
	if setup != nil {
		setup(r)
	}
	s := &observedScreen{SimulationScreen: tcell.NewSimulationScreen("UTF-8"), frames: make(chan string, 128)}
	ui := &questionTestUI{screen: s, runtime: r, done: make(chan error, 1), questionEvents: questionEvents, cancel: cancel}
	f := Frontend{Runtime: r, Events: events, Screen: s, Output: io.Discard, Models: []llm.ModelSpec{selection.Model}, EditInput: editor}
	if len(sinks) > 0 {
		f.Graphics = sinks[0]
	}
	stopped := make(chan struct{})
	go func() { ui.done <- f.Run(ctx); close(stopped) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Error("frontend did not stop")
		}
		r.Close()
	})
	ui.wait(t, "/help")
	s.SetSize(100, 30)
	s.PostEventWait(tcell.NewEventResize(100, 30))
	return ui
}

func (u *questionTestUI) wait(t *testing.T, needle string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	var latest string
	for {
		select {
		case latest = <-u.screen.frames:
			if strings.Contains(latest, needle) {
				return latest
			}
		case <-deadline:
			t.Fatalf("missing %q: %s", needle, latest)
		}
	}
}
func (u *questionTestUI) typeText(text string) {
	for _, r := range text {
		u.screen.PostEventWait(tcell.NewEventKey(tcell.KeyRune, r, 0))
	}
}
func (u *questionTestUI) key(k tcell.Key) { u.screen.PostEventWait(tcell.NewEventKey(k, 0, 0)) }

func questionScript() []llm.ScriptResponse {
	return []llm.ScriptResponse{{Calls: []llm.ToolCall{{ID: "q", Name: "question", Arguments: []byte(`{"questions":[{"id":"choice","prompt":"Choose a method?","recommended_option_id":"b","options":[{"id":"a","label":"First"},{"id":"b","label":"Second"}]},{"id":"notes","prompt":"Add notes?"}]}`)}}}, {Text: "Done answering."}}
}

func TestQuestionFreeTextEnterAdvancesInFrontend(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: questionScript()})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEnd)
	u.key(tcell.KeyEnter)
	u.typeText("custom method")
	u.key(tcell.KeyEnter)
	u.wait(t, "Add notes?")
	u.typeText("custom notes")
	u.key(tcell.KeyEnter)
	u.wait(t, "[Submit]")
	if u.runtime.PendingQuestion() == nil {
		t.Fatal("finishing the last answer submitted without review")
	}
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	if u.runtime.PendingQuestion() != nil {
		t.Fatal("explicit Submit did not finish the question")
	}
}

func TestDismissedQuestionNormalInputRedirectsWithoutAnotherTurn(t *testing.T) {
	for _, modifier := range []tcell.ModMask{0, tcell.ModAlt} {
		t.Run(fmt.Sprint(modifier), func(t *testing.T) {
			responses := questionScript()
			responses[1] = llm.ScriptResponse{Prefix: "user: Do something else", Text: "Redirect received."}
			u := newQuestionTestUI(t, &llm.Script{Responses: responses})
			u.typeText("ask")
			u.key(tcell.KeyEnter)
			u.wait(t, "Choose a method?")
			u.key(tcell.KeyEscape)
			frame := u.wait(t, "What would you like to do instead?")
			if strings.Contains(frame, "Waiting for answer") {
				t.Fatal("stale question wait after dismissal", frame)
			}
			form := u.runtime.PendingQuestion()
			if form == nil || !form.Dismissed {
				t.Fatal(form)
			}
			// Local inspection must leave the form reopenable and not resume inference.
			u.typeText("/jobs")
			u.key(tcell.KeyEnter)
			u.wait(t, "jobs · Esc closes")
			if u.runtime.PendingQuestion() == nil {
				t.Fatal("local command settled the question")
			}
			u.key(tcell.KeyEscape)
			u.typeText("Do something else")
			u.screen.PostEventWait(tcell.NewEventKey(tcell.KeyEnter, 0, modifier))
			u.wait(t, "Redirect received.")
			u.wait(t, "Turn complete")
			messages, err := u.runtime.Store.Messages(u.runtime.Current())
			if err != nil {
				t.Fatal(err)
			}
			redirects, resultAt, inputAt := 0, -1, -1
			for i, m := range messages {
				if m.Role == "tool" && m.CallID == "q" {
					var result struct {
						Dismissed bool            `json:"dismissed"`
						Answers   json.RawMessage `json:"answers"`
					}
					if err := json.Unmarshal([]byte(m.Content), &result); err != nil || !result.Dismissed || len(result.Answers) != 0 {
						t.Fatal("wrong question result", m.Content, err)
					}
					resultAt = i
				}
				if m.Role == "user" && !m.Runtime && m.Content == "Do something else" {
					redirects++
					inputAt = i
				}
			}
			if redirects != 1 || resultAt < 0 || inputAt <= resultAt || u.runtime.PendingQuestion() != nil {
				t.Fatal("redirect lost/duplicated or out of order", messages)
			}
		})
	}
}

func TestDismissedQuestionReopensWithDraftAndCanStillBeAnswered(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: questionScript()})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEnd)
	u.key(tcell.KeyEnter)
	u.typeText("saved draft")
	u.key(tcell.KeyEscape) // Leaves custom text editing.
	u.key(tcell.KeyEscape) // Dismisses the dialog, without completing its call.
	u.wait(t, "What would you like to do instead?")
	u.typeText("/questions")
	u.key(tcell.KeyEnter)
	u.wait(t, "Text: saved draft")
	form := u.runtime.PendingQuestion()
	if form == nil || form.Dismissed {
		t.Fatal("reopening did not restore the pending form", form)
	}
	if err := u.runtime.AnswerQuestion(form.ID, []session.Answer{{ID: "choice", Values: []string{"saved draft"}, Source: "custom"}, {ID: "notes", Values: []string{"notes"}, Source: "custom"}}); err != nil {
		t.Fatal(err)
	}
	u.wait(t, "Turn complete")
	if count, _ := u.runtime.SteeringPreview(0); count != 0 {
		t.Fatal("reopening queued a redirect")
	}
}

func TestQuestionArrivingDuringModelPickerOpensAfterPickerCloses(t *testing.T) {
	p := &questionTestProvider{Script: llm.Script{Responses: questionScript()}, ready: make(chan struct{}), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not start")
	}
	u.key(tcell.KeyCtrlX)
	u.typeText("m")
	u.wait(t, "Model family")
	close(p.release)
	select {
	case <-u.questionEvents:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not ask its question")
	}
	// The event channel is ordered. A later status line visible to the left of
	// the picker proves the question was handled while that picker was open.
	u.runtime.Emit(session.Event{Kind: "status", Text: "REVIEW_BARRIER", SessionID: u.runtime.Current(), Generation: u.runtime.Generation()})
	u.wait(t, "RE│")
	u.key(tcell.KeyEscape)
	u.wait(t, "Second (Recommended)")
	if u.runtime.PendingQuestion() == nil {
		t.Fatal("opening the deferred dialog changed pending state")
	}
}

func TestQuestionFrontendSubmitPreservesComposerAndRecallsPrompts(t *testing.T) {
	p := &questionTestProvider{Script: llm.Script{Responses: questionScript()}, ready: make(chan struct{}), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.key(tcell.KeyCtrlD) // Empty composer must remain open.
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("Ctrl+D exited")
	}
	u.typeText("unfinished draft")
	u.wait(t, "> unfinished draft")
	close(p.release)
	u.wait(t, "Second (Recommended)")
	u.key(tcell.KeyEnter)
	u.wait(t, "Add notes?")
	u.screen.PostEventWait(tcell.NewEventPaste(true))
	u.typeText("custom notes")
	u.key(tcell.KeyTab)
	u.key(tcell.KeyEnter)
	u.typeText("second line")
	u.screen.PostEventWait(tcell.NewEventPaste(false))
	u.key(tcell.KeyTab)
	u.wait(t, "Submit answers")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	u.wait(t, "> unfinished draft")
	u.key(tcell.KeyUp)
	u.wait(t, "> ask")
	u.key(tcell.KeyDown)
	u.wait(t, "> unfinished draft")
	u.key(tcell.KeyCtrlC)
	select {
	case err := <-u.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ctrl+C did not exit")
	}
	messages, err := u.runtime.Store.Messages(u.runtime.Current())
	if err != nil || len(messages) != 5 || !strings.Contains(messages[3].Content, "custom notes") {
		t.Fatal(messages, err)
	}
}

func TestQuestionFrontendSequentialRoundsPreserveNextDraft(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{
		{Calls: []llm.ToolCall{
			{ID: "qa", Name: "question", Arguments: []byte(`{"questions":[{"id":"answer","prompt":"Alpha?"}]}`)},
			{ID: "qb", Name: "question", Arguments: []byte(`{"questions":[{"id":"answer","prompt":"Beta?"}]}`)},
		}},
		{Text: "Finished answering."},
	}})
	u.typeText("ask both")
	u.key(tcell.KeyEnter)
	u.wait(t, "Alpha?")
	first := u.runtime.PendingQuestion()
	if first == nil || first.Questions[0].Prompt != "Alpha?" {
		t.Fatal("first round not pending", first)
	}
	u.typeText("first answered")
	u.key(tcell.KeyTab)
	u.wait(t, "Submit answers")
	u.key(tcell.KeyEnter)
	u.wait(t, "Beta?")
	second := u.runtime.PendingQuestion()
	if second == nil || second.ID == first.ID || second.Questions[0].Prompt != "Beta?" {
		t.Fatal("second round not pending", second)
	}
	u.typeText("next draft")
	u.wait(t, "Text: next draft")
	// Late initial publication and closure for the previous round must not
	// clear the current dialog or replace its retained draft.
	u.runtime.Emit(session.Event{Kind: "question", Question: first, SessionID: u.runtime.Current(), Generation: u.runtime.Generation()})
	u.runtime.Emit(session.Event{Kind: "question_closed", Question: first, SessionID: u.runtime.Current(), Generation: u.runtime.Generation()})
	u.runtime.Emit(session.Event{Kind: "status", Text: "◆◆ previous round closed", SessionID: u.runtime.Current(), Generation: u.runtime.Generation()})
	if frame := u.wait(t, "◆◆"); !strings.Contains(frame, "Text: next draft") {
		t.Fatal("previous round closure lost the next draft", frame)
	}
	u.key(tcell.KeyTab)
	u.wait(t, "Submit answers")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	if u.runtime.PendingQuestion() != nil {
		t.Fatal("answered round still pending")
	}
	messages, err := u.runtime.Store.Messages(u.runtime.Current())
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Role == "tool" && message.CallID == "qb" && strings.Contains(message.Content, "next draft") {
			return
		}
	}
	t.Fatal("second answer lost", messages)
}

func TestComposerCtrlDScrollsDownWithoutExiting(t *testing.T) {
	var rows []string
	for i := range 80 {
		rows = append(rows, fmt.Sprintf("ROW_%03d", i))
	}
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{{Text: "```\n" + strings.Join(rows, "\n") + "\n```"}}})
	u.typeText("show rows")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	for range 10 {
		u.key(tcell.KeyCtrlU)
	}
	u.wait(t, "ROW_000")
	for range 10 {
		u.key(tcell.KeyCtrlD)
	}
	u.wait(t, "ROW_079")
	u.key(tcell.KeyCtrlC)
	select {
	case <-u.done:
	case <-time.After(3 * time.Second):
		t.Fatal("scrolling exited or blocked")
	}
}

func TestQuestionFrontendDismissReopenAndCtrlCExitsForm(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: questionScript()})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Second (Recommended)")
	u.key(tcell.KeyEnter)
	u.wait(t, "Add notes?")
	u.typeText("remember this")
	u.key(tcell.KeyEscape)
	u.key(tcell.KeyEscape)
	u.typeText("/questions")
	u.key(tcell.KeyEnter)
	u.wait(t, "Text: remember this")
	u.key(tcell.KeyCtrlD)
	u.key(tcell.KeyCtrlC)
	select {
	case err := <-u.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("exit deadlocked pending question")
	}
	if u.runtime.PendingQuestion() != nil {
		t.Fatal("pending form survived exit")
	}
	var raw string
	if err := u.runtime.Store.DB.QueryRow("SELECT result_json FROM tool_calls WHERE name='question'").Scan(&raw); err != nil || !strings.Contains(raw, "cancelled") {
		t.Fatal(raw, err)
	}
}
