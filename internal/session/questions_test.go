package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	contextbuild "scicode/internal/context"
	"scicode/internal/provider"
	"scicode/internal/tool"
)

func TestQuestionRejectsNonMainExecution(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main/child"}, "question", json.RawMessage(`{"questions":[{"id":"q","prompt":"Missing information?"}]}`))
	if !strings.Contains(string(record.Result), "available only to the main agent") || r.PendingQuestion() != nil {
		t.Fatal("non-main execution created a question", string(record.Result), r.PendingQuestion())
	}
}

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

func TestQuestionRejectsConcurrentMainInvocation(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	argument := json.RawMessage(`{"questions":[{"id":"first","prompt":"First?"},{"id":"second","prompt":"Second?"},{"id":"third","prompt":"Third?"}]}`)
	_, ids := batchIntents(t, r, "main", []provider.ToolCall{{ID: "first", Name: "question", Arguments: argument}, {ID: "second", Name: "question", Arguments: argument}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan tool.Record, 1)
	go func() {
		done <- r.Tools.Invoke(ctx, tool.Execution{Actor: "main", CallID: ids[0]}, "question", argument)
	}()
	var view *QuestionForm
	for view == nil {
		if event := receive(t, events); event.Kind == "question" {
			view = event.Question
		}
	}
	record := r.Tools.Invoke(ctx, tool.Execution{Actor: "main", CallID: ids[1]}, "question", argument)
	if !strings.Contains(string(record.Result), "a main question is already pending") {
		t.Fatal("concurrent main invocation did not fail", string(record.Result))
	}
	if pending := r.PendingQuestion(); pending == nil || pending.ID != view.ID || len(pending.Questions) != 3 {
		t.Fatal("rejected invocation replaced the pending round", pending)
	}
	answers := []Answer{{ID: "first", Source: "custom", Values: []string{"one"}}, {ID: "second", Source: "custom", Values: []string{"two"}}, {ID: "third", Source: "custom", Values: []string{"three"}}}
	for _, invalid := range [][]Answer{
		answers[:2],
		{answers[1], answers[0], answers[2]},
		{answers[0], answers[1], {ID: "third", Source: "invalid", Values: []string{"three"}}},
	} {
		if err := r.AnswerQuestion(view.ID, invalid); err == nil {
			t.Fatal("invalid round answer accepted", invalid)
		}
	}
	if err := r.AnswerQuestion(view.ID, answers); err != nil {
		t.Fatal(err)
	}
	var reply questionReply
	if result := receive(t, done).Result; json.Unmarshal(result, &reply) != nil || len(reply.Answers) != 3 || reply.Dismissed {
		t.Fatal("whole round did not complete", string(result))
	}
	if r.PendingQuestion() != nil {
		t.Fatal("completed invocation retained its pending form")
	}
}

func TestQuestionAnswersRequireExactlyOneValue(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	view := QuestionForm{ID: "form", Questions: []Question{{ID: "q", Prompt: "Q?", Options: []Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}}}}
	form := &questionForm{view: view, reply: make(chan questionReply, 1)}
	r.questions.pending = form
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
	if got := <-form.reply; got.Dismissed || len(got.Answers) != 1 || len(got.Answers[0].Values) != 1 || got.Answers[0].Values[0] != "b" {
		t.Fatal(got)
	}
}

func TestQuestionAnswerRejectsDuplicateAfterChannelConsumed(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	view := QuestionForm{ID: "form", Questions: []Question{{ID: "q", Prompt: "Q?"}}}
	form := &questionForm{view: view, reply: make(chan questionReply, 1)}
	r.questions.pending = form
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
	if r.PendingQuestion() != nil {
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
	pending := r.PendingQuestion()
	pending.ID = "changed"
	pending.Questions[0].ID = "changed"
	pending.Questions[0].Options[0].ID = "changed"
	view.Questions[0].Options[1].ID = "changed"
	if got := r.PendingQuestion(); got.ID == "changed" || got.Questions[0].ID != "q" || got.Questions[0].Options[0].ID != "a" || got.Questions[0].Options[1].ID != "b" {
		t.Fatal("view modified validation state")
	}
	r.Interrupt()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("question did not cancel")
	}
	if r.PendingQuestion() != nil {
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

func TestDismissedQuestionRedirectIncludesResultAndHumanInputAtNextRequest(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	requests := make(chan provider.Request, 2)
	calls := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		calls++
		requests <- req
		if calls == 1 {
			return emit(provider.StreamEvent{Kind: "call", Call: &provider.ToolCall{ID: "q", Name: "question", Arguments: []byte(`{"questions":[{"id":"q","prompt":"Choose?"}]}`)}})
		}
		return emit(provider.StreamEvent{Kind: "text", Text: "Redirect received"})
	}}
	done := make(chan error, 1)
	go func() { done <- r.Run(&provider.Message{Role: "user", Content: "ask"}) }()
	<-requests
	var form QuestionForm
	for form.ID == "" {
		select {
		case event := <-events:
			if event.Kind == "question" {
				form = *event.Question
			}
		case <-time.After(3 * time.Second):
			t.Fatal("question did not arrive")
		}
	}
	if err := r.DismissQuestion(form.ID); err != nil {
		t.Fatal(err)
	}
	if pending := r.PendingQuestion(); pending == nil || !pending.Dismissed {
		t.Fatal("dismissal completed instead of hiding the form", pending)
	}
	select {
	case request := <-requests:
		t.Fatal("dismissal resumed inference without human input", request)
	default:
	}
	redirect := contextbuild.Input{Text: "Do something else instead"}
	if accepted, err := r.RedirectDismissedQuestion(redirect); err != nil || !accepted {
		t.Fatal(accepted, err)
	}
	select {
	case req := <-requests:
		resultAt, humanAt := -1, -1
		for i, m := range req.Messages {
			if m.Role == "tool" && m.CallID == "q" {
				var result questionReply
				if err := json.Unmarshal([]byte(m.Content), &result); err != nil || !result.Dismissed || len(result.Answers) != 0 {
					t.Fatal("incorrect dismissal result", m.Content, err)
				}
				resultAt = i
			}
			if m.Role == "user" && !m.Runtime && m.Content == redirect.Text {
				humanAt = i
			}
		}
		if resultAt < 0 || humanAt <= resultAt {
			t.Fatal("redirect missing or preceding tool settlement", req.Messages)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dismissed question blocked redirect")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("redirect turn did not finish")
	}
	if count, _ := r.SteeringPreview(0); r.PendingQuestion() != nil || count != 0 {
		t.Fatal("redirect left live input behind")
	}
}

func TestQuestionDismissReopenAndResolution(t *testing.T) {
	for _, resolution := range []string{"answer", "redirect"} {
		t.Run(resolution, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "ask")
			message := contextbuild.Input{Text: "redirect"}
			if r.PendingQuestion() != nil {
				t.Fatal("new runtime has a pending question")
			}
			if accepted, err := r.RedirectDismissedQuestion(message); err != nil || accepted {
				t.Fatal("input without a question should use ordinary queuing", accepted, err)
			}
			form := &questionForm{view: QuestionForm{ID: "form", Questions: []Question{{ID: "q", Prompt: "Q?"}}}, reply: make(chan questionReply, 1)}
			r.questions.pending = form
			answer := []Answer{{ID: "q", Source: "custom", Values: []string{"answer"}}}
			if err := r.AnswerQuestion("wrong", answer); err == nil {
				t.Fatal("wrong form ID answered")
			}
			for _, change := range []func(string) error{r.DismissQuestion, r.ReopenQuestion} {
				if err := change("wrong"); err == nil {
					t.Fatal("wrong form ID changed dismissal")
				}
			}
			if accepted, err := r.RedirectDismissedQuestion(message); err != nil || accepted {
				t.Fatal("open question redirected", accepted, err)
			}
			for _, change := range []func(string) error{r.DismissQuestion, r.DismissQuestion, r.ReopenQuestion, r.ReopenQuestion} {
				if err := change("form"); err != nil {
					t.Fatal(err)
				}
			}
			if pending := r.PendingQuestion(); pending == nil || pending.Dismissed {
				t.Fatal("reopening did not clear dismissal", pending)
			}
			if accepted, err := r.RedirectDismissedQuestion(message); err != nil || accepted {
				t.Fatal("reopened question redirected", accepted, err)
			}
			if err := r.DismissQuestion("form"); err != nil {
				t.Fatal(err)
			}
			if accepted, err := r.RedirectDismissedQuestion(contextbuild.Input{}); err == nil || accepted {
				t.Fatal("empty input settled a user dismissal", accepted, err)
			}
			count, _ := r.SteeringPreview(0)
			if pending := r.PendingQuestion(); pending == nil || !pending.Dismissed || len(form.reply) != 0 || count != 0 {
				t.Fatal("rejected input changed the pending form", pending)
			}
			if resolution == "answer" {
				if err := r.AnswerQuestion("form", answer); err != nil {
					t.Fatal(err)
				}
				answer[0].Values[0] = "mutated"
				if got := <-form.reply; got.Dismissed || len(got.Answers) != 1 || got.Answers[0].Values[0] != "answer" {
					t.Fatal("answer reply did not own its values", got)
				}
			} else {
				if accepted, err := r.RedirectDismissedQuestion(message); err != nil || !accepted {
					t.Fatal("dismissed main question could not redirect", accepted, err)
				}
				if got := <-form.reply; !got.Dismissed || len(got.Answers) != 0 {
					t.Fatal(got)
				}
			}
			if r.PendingQuestion() != nil {
				t.Fatal("resolved form still pending")
			}
			if err := r.AnswerQuestion("form", answer); err == nil {
				t.Fatal("resolved form answered after reply consumption")
			}
			for _, change := range []func(string) error{r.DismissQuestion, r.ReopenQuestion} {
				if err := change("form"); err == nil {
					t.Fatal("resolved form changed dismissal")
				}
			}
			if accepted, err := r.RedirectDismissedQuestion(message); err != nil || accepted {
				t.Fatal("resolved form redirected twice", accepted, err)
			}
			count, steers := r.SteeringPreview(1)
			if resolution == "redirect" {
				if count != 1 || steers[0] != message.Text {
					t.Fatal("human input lost or duplicated", steers)
				}
			} else if count != 0 {
				t.Fatal("answer queued a redirect", steers)
			}
		})
	}
}
