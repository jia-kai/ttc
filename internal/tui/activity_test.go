package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/llm"
	"ttc/internal/session"
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
	activity.observe(session.Event{Actor: "main/child", Kind: "assistant"}, now)
	activity.observe(session.Event{Actor: "main/child", Retry: &llm.Retry{DelayMilliseconds: 10000}}, now)
	check(now, "Working · 30s")
	activity.syncQuestion(&session.QuestionForm{ID: "first"}, now)
	check(now.Add(time.Second), "Waiting for answer · 1s")
	activity.syncQuestion(nil, now.Add(time.Second))
	activity.syncQuestion(&session.QuestionForm{ID: "second"}, now.Add(time.Second))
	check(now.Add(2*time.Second), "Waiting for answer · 1s")
	// Late closure of the previous round must not clear the current wait.
	activity.observe(session.Event{Kind: "question_closed", Question: &session.QuestionForm{ID: "first"}}, now.Add(2*time.Second))
	check(now.Add(3*time.Second), "Waiting for answer · 2s")
	activity.syncQuestion(nil, now.Add(3*time.Second))
	check(now.Add(3*time.Second), "Working · 33s")
	activity.observe(session.Event{Actor: "main", Retry: &llm.Retry{DelayMilliseconds: 10000}}, now.Add(3*time.Second))
	check(now.Add(5*time.Second), "Retrying · 2s")
	check(now.Add(13*time.Second), "Working · 43s")
	activity.observe(session.Event{Actor: "main", Kind: "delta"}, now.Add(6*time.Second))
	check(now.Add(6*time.Second), "Working · 36s")
}

func TestQuestionActivitySyncReopenAndIgnoresStaleEvents(t *testing.T) {
	at := time.Unix(1000, 0)
	var activity turnActivity
	form := &session.QuestionForm{ID: "main-form"}
	activity.syncQuestion(form, at)
	form.Dismissed = true
	activity.syncQuestion(form, at.Add(time.Second))
	activity.observe(session.Event{Kind: "question", Actor: "main", Question: &session.QuestionForm{ID: form.ID}}, at.Add(time.Second))
	if got := activity.indicator(at.Add(time.Second), at); got != "What would you like to do instead?" {
		t.Fatal("stale initial event changed dismissal", got)
	}
	form.Dismissed = false
	activity.syncQuestion(form, at.Add(2*time.Second))
	if got := activity.indicator(at.Add(2*time.Second), at); got != "Waiting for answer · 2s" {
		t.Fatal("reopened question did not restore waiting status", got)
	}
	activity.syncQuestion(nil, at.Add(3*time.Second))
	activity.observe(session.Event{Kind: "question", Actor: "main", Question: form}, at.Add(4*time.Second))
	if got := activity.indicator(at.Add(4*time.Second), at); got != "Working · 4s" {
		t.Fatal("late initial event revived a closed question", got)
	}
}

type activityRetryProvider struct {
	llm.Script
	release, finish chan struct{}
}

func (p *activityRetryProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	if err := emit(llm.StreamEvent{Kind: "retry", Retry: &llm.Retry{Attempt: 2, DelayMilliseconds: 10000, Reason: "HTTP 503"}}); err != nil {
		return err
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := emit(llm.StreamEvent{Kind: "text", Text: "Recovered"}); err != nil {
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
	u := newQuestionTestUI(t, &llm.Script{Responses: questionScript()})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEsc)
	frame := u.wait(t, "What would you like to do instead?")
	if strings.Contains(frame, "Waiting for answer") {
		t.Fatal("dismissed question still presented as waiting", frame)
	}
	form := u.runtime.PendingQuestion()
	if form == nil {
		t.Fatal("no pending question")
	}
	if err := u.runtime.AnswerQuestion(form.ID, []session.Answer{{ID: "choice", Source: "option", Values: []string{"a"}}, {ID: "notes", Source: "custom", Values: []string{"notes"}}}); err != nil {
		t.Fatal(err)
	}
	u.wait(t, "Turn complete")
}

func TestSessionNameEventReadsPersistedManualTitle(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{{Text: "Done."}}})
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
