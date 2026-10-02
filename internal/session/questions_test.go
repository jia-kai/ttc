package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"scicode/internal/provider"
	"scicode/internal/tool"
)

func TestQuestionRecommendationValidation(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	for _, definition := range r.Tools.Definitions() {
		if definition.Name == "question" && (strings.Contains(string(definition.Parameters), `"multiple"`) || !strings.Contains(definition.Description, "single-choice")) {
			t.Fatal("model contract does not enforce single-choice", definition)
		}
	}
	for _, argument := range []string{
		`{"questions":[{"id":"q","prompt":"Q?","multiple":true}]}`,
		`{"questions":[{"id":"q","prompt":"Q?","multiple":false}]}`,
		`{"questions":[{"id":"q","prompt":"Q?","recommended_option_id":"missing","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}]}]}`,
		`{"questions":[{"id":"q","prompt":"Q?","recommended_option_id":"a"}]}`,
	} {
		record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main"}, "question", json.RawMessage(argument))
		if !strings.Contains(string(record.Result), "invalid_input") {
			t.Fatal(string(record.Result))
		}
	}
}

func TestQuestionAnswersRequireExactlyOneValue(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	view := QuestionForm{ID: "form", Actor: "main", Questions: []Question{{ID: "q", Prompt: "Q?", Options: []Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}}}}
	form := questionForm{view: view, reply: make(chan []Answer, 1)}
	r.questions.forms[view.ID] = form
	for _, answer := range []Answer{
		{ID: "q", Source: "option"},
		{ID: "q", Source: "option", Values: []string{"a", "b"}},
		{ID: "q", Source: "option", Values: []string{"a", "a"}},
		{ID: "q", Source: "custom", Values: []string{"one", "two"}},
		{ID: "q", Source: "option", Values: []string{"missing"}},
	} {
		if err := r.AnswerQuestion(view.ID, []Answer{answer}); err == nil {
			t.Fatal("invalid answer accepted", answer)
		}
		if len(form.reply) != 0 {
			t.Fatal("invalid answer completed the form")
		}
	}
	answer := []Answer{{ID: "q", Source: "option", Values: []string{"b"}}}
	if err := r.AnswerQuestion(view.ID, answer); err != nil {
		t.Fatal(err)
	}
	if got := <-form.reply; len(got) != 1 || len(got[0].Values) != 1 || got[0].Values[0] != "b" {
		t.Fatal(got)
	}
}

func TestQuestionAnswerRejectsDuplicateAfterChannelConsumed(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	view := QuestionForm{ID: "form", Actor: "main", Questions: []Question{{ID: "q", Prompt: "Q?"}}}
	form := questionForm{view: view, reply: make(chan []Answer, 1)}
	r.questions.forms[view.ID] = form
	for _, value := range []string{" ", strings.Repeat("x", MaxAnswerBytes+1), "\xff"} {
		if err := r.AnswerQuestion(view.ID, []Answer{{ID: "q", Values: []string{value}, Source: "custom"}}); err == nil {
			t.Fatal("invalid custom accepted")
		}
	}
	answer := []Answer{{ID: "q", Values: []string{"yes"}, Source: "custom"}}
	if err := r.AnswerQuestion(view.ID, answer); err != nil {
		t.Fatal(err)
	}
	<-form.reply
	if err := r.AnswerQuestion(view.ID, answer); err == nil {
		t.Fatal("duplicate accepted after channel consumption")
	}
	if len(r.PendingQuestions()) != 0 {
		t.Fatal("answered form still pending")
	}
}

func TestTypedQuestionLifecycleAndCancellation(t *testing.T) {
	r, events := runtimeFixture(t, []provider.ScriptResponse{{Calls: []provider.ToolCall{{ID: "q", Name: "question", Arguments: []byte(`{"questions":[{"id":"q","prompt":"Pick?","recommended_option_id":"b","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}]}]}`)}}}})
	done := make(chan error, 1)
	go func() { m := provider.Message{Role: "user", Content: "ask"}; done <- r.Run(&m) }()
	var view *QuestionForm
	deadline := time.After(3 * time.Second)
	for view == nil {
		select {
		case event := <-events:
			if event.Kind == "question" {
				view = event.Question
			}
		case <-deadline:
			t.Fatal("question did not arrive")
		}
	}
	if view.ID == "" || view.EntryID == 0 || view.Questions[0].RecommendedOptionID != "b" {
		t.Fatal(view)
	}
	pending := r.PendingQuestions()
	pending[0].Questions[0].Options[0].ID = "changed"
	if r.PendingQuestions()[0].Questions[0].Options[0].ID != "a" {
		t.Fatal("view modified validation state")
	}
	r.Interrupt()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("question did not cancel")
	}
	if len(r.PendingQuestions()) != 0 {
		t.Fatal("stale pending question")
	}
	closed := false
	for len(events) > 0 {
		e := <-events
		if e.Kind == "question_closed" && e.Question.ID == view.ID {
			closed = true
		}
	}
	if !closed {
		t.Fatal("missing typed closure")
	}
}
