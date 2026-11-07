package session

import (
	"reflect"
	"testing"
	"time"
)

func TestCoordinatorQuestionControlsCommitInMailboxOrderWhileEffectBlocked(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	form := &questionForm{
		view:  QuestionForm{ID: "form", Questions: []Question{{ID: "answer", Prompt: "Answer?"}}},
		reply: make(chan questionReply, 1),
	}
	r.questions.pending = form
	entered, release := make(chan struct{}), make(chan struct{})
	c := startCoordinator(t, r, ControlEffects{Reconnect: func() error {
		close(entered)
		<-release
		return nil
	}})
	defer close(release)
	coordinatorSubmit(t, c, Request{Kind: RequestReconnect})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("slow control effect did not start")
	}
	answers := []Answer{{ID: "answer", Source: "custom", Values: []string{"preserved"}}}
	var want []uint64
	for _, request := range []Request{
		{Kind: RequestDismissQuestion, ID: "form"},
		{Kind: RequestReopenQuestion, ID: "form"},
		{Kind: RequestDismissQuestion, ID: "form"},
		{Kind: RequestAnswer, ID: "form", Answers: answers},
	} {
		want = append(want, coordinatorSubmit(t, c, request))
	}
	var got []uint64
	for range want {
		result := coordinatorUpdate(t, c, func(update Update) bool { return update.Result != nil }).Result
		if result.Err != nil {
			t.Fatal("question control failed", result.Kind, result.Err)
		}
		got = append(got, result.RequestID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("question controls did not commit in mailbox order", got, want)
	}
	select {
	case reply := <-form.reply:
		if reply.Dismissed || !reflect.DeepEqual(reply.Answers, answers) {
			t.Fatal("answer did not complete the question", reply)
		}
	default:
		t.Fatal("question controls waited for the slow effect")
	}
}
