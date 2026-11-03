package tui

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
	contextbuild "ttc/internal/context"
	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/session"
)

type awaitSecondGateProvider struct {
	questionTestProvider
	requests                int
	resumed, releaseResumed chan struct{}
}

func (p *awaitSecondGateProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	p.requests++
	if p.requests == 2 {
		close(p.resumed)
		select {
		case <-p.releaseResumed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.questionTestProvider.Stream(ctx, req, emit)
}

func TestQueuedInputBeforeAwaitResumesSameTurnInFIFOOrder(t *testing.T) {
	p := &questionTestProvider{Script: llm.Script{Responses: []llm.ScriptResponse{
		{Calls: []llm.ToolCall{{ID: "park", Name: "await", Arguments: []byte(`{}`)}}},
		{Prefix: "user: queued second", Text: "Queued await input accepted"},
	}}, ready: make(chan struct{}), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("start")
	u.key(tcell.KeyEnter)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("inference did not start")
	}
	// Both messages enter the ordinary FIFO while inference is still busy, not
	// while Awaiting is already visible to submitPrompt.
	for _, text := range []string{"queued first", "queued second"} {
		u.typeText(text)
		u.screen.EventQ() <- tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModAlt)
		u.wait(t, "Queued · "+text)
	}
	close(p.release)
	// There are no timers, job exits, deadlines or additional user inputs that
	// could accidentally release the indefinite await.
	u.wait(t, "Queued await input accepted")
	u.wait(t, "Turn complete")
	messages, err := u.runtime.Store.Messages(u.runtime.Current())
	if err != nil {
		t.Fatal(err)
	}
	var humans []string
	for _, message := range messages {
		if message.Type == llm.TypeUserInput {
			humans = append(humans, message.Content)
		}
	}
	if len(humans) != 3 || humans[0] != "start" || humans[1] != "queued first" || humans[2] != "queued second" {
		t.Fatal("queued input lost, reordered or combined", humans)
	}
	var turns int
	if err := historytest.DB(t, u.runtime.Store).QueryRow(`SELECT count(DISTINCT turn_id) FROM model_requests WHERE purpose='coding'`).Scan(&turns); err != nil || turns != 1 {
		t.Fatal("await input launched another Run", turns, err)
	}
}

func TestQueuedSteeringRetainsOwnershipUntilAccepted(t *testing.T) {
	inputs := []contextbuild.Input{{Text: "first", Attachments: []contextbuild.Attachment{{Path: "note", Kind: "file", Text: "attached evidence"}}}, {Text: "second"}}
	p := &helpTestProvider{requests: make(chan llm.Request, 4), release: make(chan struct{})}
	c := newAsyncTestController(t, p, nil)
	c.submit(frontendSubmission{ready: true, text: "active task"})
	waitControllerState(t, c, func(state session.CoordinatorState) bool { return state.Busy })
	for i, input := range inputs {
		if _, err := c.coordinator.Submit(session.Request{Kind: session.RequestInput, Generation: c.observedGeneration, OperationID: c.observedOperationID, Input: input}); err != nil {
			t.Fatal(err)
		}
		if result := waitControllerResult(t, c, session.RequestInput); result.Err != nil {
			t.Fatal(result.Err)
		}
		waitControllerState(t, c, func(state session.CoordinatorState) bool { return len(state.Queue) == i+1 })
	}
	newest := contextbuild.Input{Text: "newest", Attachments: []contextbuild.Attachment{{Path: "new", Kind: "file", Text: "new snapshot"}}}
	if _, err := c.coordinator.Submit(session.Request{Kind: session.RequestInput, Generation: c.observedGeneration + 1, OperationID: c.observedOperationID, Input: newest, Steer: true}); err != nil {
		t.Fatal(err)
	}
	result := waitControllerResult(t, c, session.RequestInput)
	if result.Err == nil || result.Restored == nil || !reflect.DeepEqual(*result.Restored, newest) || len(c.queue) != len(inputs) {
		t.Fatal("failed steering lost input ownership or changed the FIFO", result, c.queue)
	}
	for i := len(inputs) - 1; i >= 0; i-- {
		c.cancelPendingInput("/cancel-queue")
		result := waitControllerResult(t, c, session.RequestCancelQueue)
		want := inputs[i]
		want.Source = "queue"
		if result.Err != nil || result.Restored == nil || !reflect.DeepEqual(*result.Restored, want) {
			t.Fatal("queue lost its original attachment snapshot", result, want)
		}
	}
}

func TestSubmissionBeforeAwaitEventDoesNotOvertakeQueuedInput(t *testing.T) {
	p := &awaitSecondGateProvider{questionTestProvider: questionTestProvider{Script: llm.Script{Responses: []llm.ScriptResponse{
		{Calls: []llm.ToolCall{{ID: "park", Name: "await", Arguments: []byte(`{}`)}}},
		{Text: "Older queued input accepted"},
		{Prefix: "user: newest", Text: "Pre-event input accepted"},
	}}, ready: make(chan struct{}), release: make(chan struct{})}, resumed: make(chan struct{}), releaseResumed: make(chan struct{})}
	parked, releaseEvent := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	u := newQuestionTestUIWithSetup(t, p, nil, func(r *session.Runtime) {
		emit := r.Emit
		r.Emit = func(event session.Event) {
			if event.Kind == "awaiting" && event.Actor == "main" {
				close(parked)
				<-releaseEvent
			}
			if emit != nil {
				emit(event)
			}
		}
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseEvent) }) })
	u.typeText("start")
	u.key(tcell.KeyEnter)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("inference did not start")
	}
	u.typeText("older queued")
	u.screen.EventQ() <- tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModAlt)
	u.wait(t, "Queued · older queued")
	close(p.release)
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("runtime did not park")
	}
	// Coordinator admission, not the delayed UI event, resumes the older FIFO.
	// Keep resumed coding active so the later input cannot race final completion.
	select {
	case <-p.resumed:
	case <-time.After(3 * time.Second):
		t.Fatal("coordinator did not resume queued await input")
	}
	u.typeText("newest")
	u.wait(t, "> newest")
	u.screen.EventQ() <- tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModAlt)
	deadline := time.After(3 * time.Second)
	for {
		select {
		case frame := <-u.screen.frames:
			if !strings.Contains(frame, "> newest") {
				goto submitted
			}
		case <-deadline:
			t.Fatal("frontend could not submit while awaiting update was delayed")
		}
	}
submitted:
	releaseOnce.Do(func() { close(releaseEvent) })
	u.wait(t, "Queued · newest")
	close(p.releaseResumed)
	u.wait(t, "Pre-event input accepted")
	u.wait(t, "Turn complete")
	messages, err := u.runtime.Store.Messages(u.runtime.Current())
	if err != nil {
		t.Fatal(err)
	}
	var humans []string
	for _, message := range messages {
		if message.Type == llm.TypeUserInput {
			humans = append(humans, message.Content)
		}
	}
	if !reflect.DeepEqual(humans, []string{"start", "older queued", "newest"}) {
		t.Fatal("pre-event input overtook the queue", humans)
	}
}
