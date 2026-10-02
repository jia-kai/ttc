package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/provider"
	"scicode/internal/session"
)

func TestTurnActivityUsesPhaseDurationsAndIgnoresChildren(t *testing.T) {
	now := time.Unix(1000, 0)
	started := now.Add(-30 * time.Second)
	var activity turnActivity
	check := func(at time.Time, want string) {
		t.Helper()
		if got := activity.indicator(at, started); got != want {
			t.Fatalf("indicator = %q, want %q", got, want)
		}
	}
	check(now, "Working · 30s")
	activity.observe(session.Event{Actor: "main/child", Kind: "question", Question: &session.QuestionForm{ID: "child", Actor: "main/child"}}, now)
	activity.observe(session.Event{Actor: "main/child", Retry: &provider.Retry{DelayMilliseconds: 10000}}, now)
	check(now, "Working · 30s")
	activity.observe(session.Event{Actor: "main", Kind: "question", Question: &session.QuestionForm{ID: "first", Actor: "main"}}, now)
	activity.observe(session.Event{Actor: "main", Kind: "question", Question: &session.QuestionForm{ID: "second", Actor: "main"}}, now.Add(time.Second))
	check(now.Add(2*time.Second), "Waiting for answer · 2s")
	activity.observe(session.Event{Kind: "question_closed", Question: &session.QuestionForm{ID: "first"}}, now.Add(2*time.Second))
	check(now.Add(3*time.Second), "Waiting for answer · 2s")
	activity.observe(session.Event{Kind: "question_closed", Question: &session.QuestionForm{ID: "second"}}, now.Add(3*time.Second))
	check(now.Add(3*time.Second), "Working · 33s")
	activity.observe(session.Event{Actor: "main", Retry: &provider.Retry{DelayMilliseconds: 10000}}, now.Add(3*time.Second))
	check(now.Add(5*time.Second), "Retrying · 2s")
	check(now.Add(13*time.Second), "Working · 43s")
	activity.observe(session.Event{Actor: "main", Kind: "delta"}, now.Add(6*time.Second))
	check(now.Add(6*time.Second), "Working · 36s")
}

type activityRetryProvider struct {
	provider.Script
	release, finish chan struct{}
}

func (p *activityRetryProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	if err := emit(provider.StreamEvent{Kind: "retry", Retry: &provider.Retry{Attempt: 2, DelayMilliseconds: 10000, Reason: "HTTP 503"}}); err != nil {
		return err
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := emit(provider.StreamEvent{Kind: "text", Text: "Recovered"}); err != nil {
		return err
	}
	select {
	case <-p.finish:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRetryActivityFrontendReturnsToWorkingOnOutput(t *testing.T) {
	p := &activityRetryProvider{release: make(chan struct{}), finish: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("retry")
	u.key(tcell.KeyEnter)
	u.wait(t, "Retrying · ")
	close(p.release)
	u.wait(t, "Recovered")
	u.wait(t, "Working · ")
	close(p.finish)
	u.wait(t, "Turn complete")
}

func TestQuestionActivityFrontendSurvivesDismissal(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{Responses: questionScript()})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEsc)
	u.wait(t, "Waiting for answer · ")
	forms := u.runtime.PendingQuestions()
	if len(forms) != 1 {
		t.Fatal(forms)
	}
	if err := u.runtime.AnswerQuestion(forms[0].ID, []session.Answer{{ID: "choice", Source: "option", Values: []string{"a"}}, {ID: "notes", Source: "custom", Values: []string{"notes"}}}); err != nil {
		t.Fatal(err)
	}
	u.wait(t, "Turn complete")
}

func TestSessionNameEventReadsPersistedManualTitle(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{Responses: []provider.ScriptResponse{{Text: "Done."}}})
	u.typeText("name this")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	if err := u.runtime.Store.RenameSession(u.runtime.Current(), "My manual title"); err != nil {
		t.Fatal(err)
	}
	u.runtime.Emit(session.Event{Kind: "session_name", Text: "Old automatic title", Generation: u.runtime.Generation(), SessionID: u.runtime.Current()})
	u.runtime.Emit(session.Event{Kind: "status", Text: "Name event processed", Generation: u.runtime.Generation(), SessionID: u.runtime.Current()})
	frame := u.wait(t, "Name event processed")
	if !strings.Contains(frame, "My manual title") || strings.Contains(frame, "Old automatic title") {
		t.Fatal(frame)
	}
}
