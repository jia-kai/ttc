package tui

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/graphics"
	"scicode/internal/history"
	"scicode/internal/provider"
	"scicode/internal/scratch"
	"scicode/internal/session"
	"scicode/internal/skills"
	"scicode/internal/workspace"
)

type questionTestProvider struct {
	provider.Script
	ready, release chan struct{}
	first          bool
}

func (p *questionTestProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
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

func newQuestionTestUI(t *testing.T, p provider.Provider, sinks ...*graphics.Kitty) *questionTestUI {
	t.Helper()
	return newQuestionTestUIWithEditor(t, p, nil, sinks...)
}

func newQuestionTestUIWithEditor(t *testing.T, p provider.Provider, editor func(context.Context, string) (string, error), sinks ...*graphics.Kitty) *questionTestUI {
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
	t.Cleanup(func() { w.Close() })
	selection := provider.Selection{Provider: "script", Model: provider.ScriptModel(), Variant: "none"}
	catalog, err := skills.Discover(context.Background(), w.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan session.Event, 64)
	questionEvents := make(chan struct{}, 8)
	r := session.New(ctx, store, w, p, selection, "", catalog, func(e session.Event) {
		select {
		case events <- e:
			if e.Kind == "question" {
				questionEvents <- struct{}{}
			}
		case <-ctx.Done():
		}
	})
	r.AutoName = false
	s := &observedScreen{SimulationScreen: tcell.NewSimulationScreen("UTF-8"), frames: make(chan string, 128)}
	ui := &questionTestUI{screen: s, runtime: r, done: make(chan error, 1), questionEvents: questionEvents, cancel: cancel}
	f := Frontend{Runtime: r, Events: events, Screen: s, Output: io.Discard, Models: []provider.ModelSpec{selection.Model}, EditInput: editor}
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

func questionScript() []provider.ScriptResponse {
	return []provider.ScriptResponse{{Calls: []provider.ToolCall{{ID: "q", Name: "question", Arguments: []byte(`{"questions":[{"id":"choice","prompt":"Choose a method?","recommended_option_id":"b","options":[{"id":"a","label":"First"},{"id":"b","label":"Second"}]},{"id":"notes","prompt":"Add notes?"}]}`)}}}, {Text: "Done answering."}}
}

func TestQuestionArrivingDuringModelPickerOpensAfterPickerCloses(t *testing.T) {
	p := &questionTestProvider{Script: provider.Script{Responses: questionScript()}, ready: make(chan struct{}), release: make(chan struct{})}
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
	if len(u.runtime.PendingQuestions()) != 1 {
		t.Fatal("opening the deferred dialog changed pending state")
	}
}

func TestQuestionFrontendSubmitPreservesComposerAndRecallsPrompts(t *testing.T) {
	p := &questionTestProvider{Script: provider.Script{Responses: questionScript()}, ready: make(chan struct{}), release: make(chan struct{})}
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
	u.key(tcell.KeyRight)
	u.wait(t, "Submit answers")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn completed")
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

type concurrentQuestionProvider struct {
	provider.Script
	mu    sync.Mutex
	steps map[string]int
}

func (p *concurrentQuestionProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	actor := "main"
	if strings.Contains(req.System, "\nYou are an isolated child agent.") {
		actor = req.Messages[0].Content
	}
	p.mu.Lock()
	step := p.steps[actor]
	p.steps[actor]++
	p.mu.Unlock()
	if actor == "main" && step == 0 {
		for _, id := range []string{"A", "B"} {
			call := provider.ToolCall{ID: id, Name: "subagent", Arguments: []byte(`{"prompt":"` + id + `","label":"` + id + `","background":true}`)}
			if err := emit(provider.StreamEvent{Kind: "call", Call: &call}); err != nil {
				return err
			}
		}
		return nil
	}
	if actor != "main" && step == 0 {
		call := provider.ToolCall{ID: "question", Name: "question", Arguments: []byte(`{"questions":[{"id":"answer","prompt":"Question ` + actor + `?"}]}`)}
		return emit(provider.StreamEvent{Kind: "call", Call: &call})
	}
	return emit(provider.StreamEvent{Kind: "text", Text: actor + " finished."})
}

func TestConcurrentQuestionClosurePreservesOtherActiveForm(t *testing.T) {
	u := newQuestionTestUI(t, &concurrentQuestionProvider{steps: map[string]int{}})
	u.typeText("ask both")
	u.key(tcell.KeyEnter)
	deadline := time.Now().Add(3 * time.Second)
	forms := u.runtime.PendingQuestions()
	for len(forms) != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		forms = u.runtime.PendingQuestions()
	}
	if len(forms) != 2 {
		t.Fatal(forms)
	}
	for range 2 {
		select {
		case <-u.questionEvents:
		case <-time.After(3 * time.Second):
			t.Fatal("question not emitted")
		}
	}
	// The two exposed left-margin characters survive the modal overlay. This
	// event follows both question events, so its frame is a UI delivery barrier.
	u.runtime.Emit(session.Event{Kind: "status", Text: "◇◇ both question events delivered", Generation: u.runtime.Generation()})
	u.wait(t, "◇◇")
	u.key(tcell.KeyEscape)
	u.key(tcell.KeyEscape)
	u.typeText("/questions " + forms[1].ID)
	u.key(tcell.KeyEnter)
	u.wait(t, forms[1].Questions[0].Prompt)
	if err := u.runtime.AnswerQuestion(forms[0].ID, []session.Answer{{ID: "answer", Values: []string{"first answered"}, Source: "custom"}}); err != nil {
		t.Fatal(err)
	}
	// Joining the first child's completion ensures its close event is queued.
	for _, job := range u.runtime.Jobs.List("main", true) {
		if job.Owner == forms[0].Actor {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, err := u.runtime.Jobs.Wait(ctx, "main", job.ID, nil)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	u.typeText("other stays active")
	u.wait(t, "Text: other stays active")
	u.key(tcell.KeyRight)
	u.wait(t, "Submit answers")
	u.key(tcell.KeyEnter)
	deadline = time.Now().Add(3 * time.Second)
	for len(u.runtime.PendingQuestions()) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(u.runtime.PendingQuestions()) != 0 {
		t.Fatal("closing first form dismissed second")
	}
	u.key(tcell.KeyCtrlC)
	select {
	case <-u.done:
	case <-time.After(3 * time.Second):
		t.Fatal("exit blocked")
	}
}

func TestComposerCtrlDScrollsDownWithoutExiting(t *testing.T) {
	var rows []string
	for i := range 80 {
		rows = append(rows, fmt.Sprintf("ROW_%03d", i))
	}
	u := newQuestionTestUI(t, &provider.Script{Responses: []provider.ScriptResponse{{Text: "```\n" + strings.Join(rows, "\n") + "\n```"}}})
	u.typeText("show rows")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn completed")
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
	u := newQuestionTestUI(t, &provider.Script{Responses: questionScript()})
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
	if len(u.runtime.PendingQuestions()) != 0 {
		t.Fatal("pending form survived exit")
	}
	var raw string
	if err := u.runtime.Store.DB.QueryRow("SELECT result_json FROM tool_calls WHERE name='question'").Scan(&raw); err != nil || !strings.Contains(raw, "cancelled") {
		t.Fatal(raw, err)
	}
}
