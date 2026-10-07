package tui

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/llm"
	"ttc/internal/session"
)

func TestQuestionEditorMultilineAnswersPreserveMainDraft(t *testing.T) {
	p := &questionTestProvider{Script: llm.Script{Responses: questionScript()}, ready: make(chan struct{}), release: make(chan struct{})}
	inputs := make(chan string, 2)
	other, notes := "edited method\nαβ", "edited notes\nsecond line"
	u := newQuestionTestUIWithEditor(t, p, func(_ context.Context, draft string) (string, error) {
		inputs <- draft
		if draft == "method draft" {
			return other, nil
		}
		return notes, nil
	})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not start")
	}
	u.typeText("unfinished draft")
	u.wait(t, "> unfinished draft")
	close(p.release)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEnd)
	u.key(tcell.KeyEnter)
	u.typeText("method draft")
	u.key(tcell.KeyCtrlX)
	u.typeText("e")
	u.wait(t, "αβ▏")
	u.key(tcell.KeyEnter)
	u.wait(t, "Add notes?")
	u.typeText("notes draft")
	u.key(tcell.KeyCtrlX)
	u.typeText("e")
	u.wait(t, "second line▏")
	for _, want := range []string{"method draft", "notes draft"} {
		select {
		case got := <-inputs:
			if got != want {
				t.Fatalf("editor input = %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("editor did not receive question draft")
		}
	}
	u.key(tcell.KeyEnter)
	u.wait(t, "[Submit]")
	if u.runtime.PendingQuestion() == nil {
		t.Fatal("editor result submitted without review")
	}
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	u.wait(t, "> unfinished draft")
	assertQuestionEditorAnswers(t, u, []session.Answer{
		{ID: "choice", Values: []string{other}, Source: "custom"},
		{ID: "notes", Values: []string{notes}, Source: "custom"},
	})
}

func TestQuestionEditorRejectedResultPreservesEditingState(t *testing.T) {
	for _, tc := range []struct {
		name, text, message string
		err                 error
	}{
		{name: "editor error", text: "discard this", err: errors.New("editor exploded"), message: "Editor failed: editor exploded"},
		{name: "oversize", text: strings.Repeat("x", session.MaxAnswerBytes+1), message: "Text is limited to"},
		{name: "invalid UTF-8", text: string([]byte{0xff}), message: "Text must be valid UTF-8."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := make(chan string, 1)
			u := newQuestionTestUIWithEditor(t, &llm.Script{Responses: questionScript()}, func(_ context.Context, draft string) (string, error) {
				inputs <- draft
				return tc.text, tc.err
			})
			u.typeText("ask")
			u.key(tcell.KeyEnter)
			u.wait(t, "Choose a method?")
			u.key(tcell.KeyEnd)
			u.key(tcell.KeyEnter)
			u.typeText("keep kill")
			u.key(tcell.KeyCtrlW)
			u.key(tcell.KeyLeft)
			u.key(tcell.KeyCtrlX)
			u.typeText("e")
			frame := u.wait(t, tc.message)
			if !strings.Contains(frame, "Choose a method?") || !strings.Contains(frame, "Text: keep▏ ") {
				t.Fatalf("error was not shown on the preserved question draft: %s", frame)
			}
			select {
			case got := <-inputs:
				if got != "keep " {
					t.Fatalf("editor input = %q", got)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("editor did not start")
			}
			// Insertion and yank prove rejection preserved both cursor and kill buffer.
			u.typeText("!")
			u.key(tcell.KeyCtrlY)
			u.wait(t, "Text: keep!kill▏ ")
			u.key(tcell.KeyEnter)
			u.wait(t, "Add notes?")
			u.typeText("notes")
			u.key(tcell.KeyEnter)
			u.wait(t, "[Submit]")
			u.key(tcell.KeyEnter)
			u.wait(t, "Turn complete")
			assertQuestionEditorAnswers(t, u, []session.Answer{
				{ID: "choice", Values: []string{"keep!kill "}, Source: "custom"},
				{ID: "notes", Values: []string{"notes"}, Source: "custom"},
			})
		})
	}
}

func TestQuestionEditorDiscardsResultAfterQuestionCloses(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	u := newQuestionTestUIWithEditor(t, &llm.Script{Responses: questionScript()}, func(ctx context.Context, _ string) (string, error) {
		close(entered)
		select {
		case <-release:
			return "stale editor answer", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	form := u.runtime.PendingQuestion()
	if form == nil {
		t.Fatal("question not pending")
	}
	u.key(tcell.KeyEnd)
	u.key(tcell.KeyEnter)
	u.typeText("question draft")
	u.key(tcell.KeyCtrlX)
	u.typeText("e")
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("question editor did not start")
	}
	answers := []session.Answer{
		{ID: "choice", Values: []string{"a"}, Source: "option"},
		{ID: "notes", Values: []string{"answered elsewhere"}, Source: "custom"},
	}
	if err := u.runtime.AnswerQuestion(form.ID, answers); err != nil {
		t.Fatal(err)
	}
	close(release)
	frame := u.wait(t, "Turn complete")
	if u.runtime.PendingQuestion() != nil || strings.Contains(frame, "Choose a method?") || strings.Contains(frame, "stale editor answer") {
		t.Fatalf("closed question was restored by editor result: %s", frame)
	}
	// Also check the result did not fall back to the main composer.
	u.typeText("fresh main draft")
	u.wait(t, "> fresh main draft")
	assertQuestionEditorAnswers(t, u, answers)
}

func TestQuestionEditorPrefixDoesNotLeakAfterClosure(t *testing.T) {
	entered := make(chan string, 1)
	u := newQuestionTestUIWithEditor(t, &llm.Script{Responses: questionScript()}, func(_ context.Context, draft string) (string, error) {
		entered <- draft
		return "unexpected main replacement", nil
	})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	form := u.runtime.PendingQuestion()
	if form == nil {
		t.Fatal("question not pending")
	}
	u.key(tcell.KeyEnd)
	u.key(tcell.KeyEnter)
	u.typeText("question draft")
	u.wait(t, "Text: question draft▏")
	u.key(tcell.KeyCtrlX)
	// Allow the question prefix to be consumed before closing it externally.
	time.Sleep(50 * time.Millisecond)
	if err := u.runtime.AnswerQuestion(form.ID, []session.Answer{
		{ID: "choice", Values: []string{"a"}, Source: "option"},
		{ID: "notes", Values: []string{"elsewhere"}, Source: "custom"},
	}); err != nil {
		t.Fatal(err)
	}
	u.wait(t, "Turn complete")
	u.typeText("e")
	u.wait(t, "> e")
	select {
	case draft := <-entered:
		t.Fatalf("closed question's prefix launched the main editor with %q", draft)
	default:
	}
}

func TestHeldQuestionEditorDrainsEventsAndJoinsOnCancellation(t *testing.T) {
	entered, canceled, exited := make(chan string, 1), make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	u := newQuestionTestUIWithEditor(t, &llm.Script{Responses: questionScript()}, func(ctx context.Context, draft string) (string, error) {
		entered <- draft
		<-ctx.Done()
		close(canceled)
		<-release
		close(exited)
		return draft, ctx.Err()
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEnd)
	u.key(tcell.KeyEnter)
	u.typeText("held answer")
	u.key(tcell.KeyCtrlX)
	u.typeText("e")
	select {
	case draft := <-entered:
		if draft != "held answer" {
			t.Fatalf("editor input = %q", draft)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("question editor did not start")
	}
	drained := make(chan struct{})
	go func() {
		for range 128 { // Exceed the bounded frontend event queue while suspended.
			u.runtime.Emit(session.Event{Generation: u.runtime.Generation(), SessionID: u.runtime.Current(), Kind: "status", Text: "While answering in editor"})
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("question editor blocked runtime event delivery")
	}
	u.cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("question editor did not receive cancellation")
	}
	select {
	case err := <-u.done:
		t.Fatalf("frontend returned before editor exited: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-u.done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("frontend did not join canceled question editor")
	}
	select {
	case <-exited:
	default:
		t.Fatal("frontend returned before editor exited")
	}
}

func assertQuestionEditorAnswers(t *testing.T, u *questionTestUI, want []session.Answer) {
	t.Helper()
	messages, err := u.runtime.Store.Messages(u.runtime.Current())
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Role != "tool" || message.CallID != "q" {
			continue
		}
		var result struct {
			Answers []session.Answer `json:"answers"`
		}
		if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result.Answers, want) {
			t.Fatalf("question answers = %#v, want %#v", result.Answers, want)
		}
		return
	}
	t.Fatal("question result missing", messages)
}
