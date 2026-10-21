package tui

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
	contextbuild "ttc/internal/context"
	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/session"
)

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
	c := frontendController{frontend: &Frontend{Runtime: &session.Runtime{}}, queue: append([]contextbuild.Input(nil), inputs...)}
	if err := c.steerQueuedInputs(contextbuild.Input{Text: "newest"}); err == nil {
		t.Fatal("inactive runtime accepted queue")
	}
	if !reflect.DeepEqual(c.queue, inputs) {
		t.Fatal("failed handoff lost queued input or attachments", c.queue)
	}
}

func TestSubmissionBeforeAwaitEventDoesNotOvertakeQueuedInput(t *testing.T) {
	p := &questionTestProvider{Script: llm.Script{Responses: []llm.ScriptResponse{
		{Calls: []llm.ToolCall{{ID: "park", Name: "await", Arguments: []byte(`{}`)}}},
		{Prefix: "user: newest", Text: "Pre-event input accepted"},
	}}, ready: make(chan struct{}), release: make(chan struct{})}
	parked, releaseEvent := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	u := newQuestionTestUIWithSetup(t, p, nil, func(r *session.Runtime) {
		emit := r.Emit
		r.Emit = func(event session.Event) {
			if event.Kind == "awaiting" && event.Actor == "main" {
				close(parked)
				<-releaseEvent
			}
			emit(event)
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
	// The live state is awaiting, but its UI event has not been delivered.
	// This submit path must transfer older FIFO entries ahead of this input.
	u.typeText("newest")
	u.screen.EventQ() <- tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModAlt)
	u.wait(t, "Steer · newest")
	releaseOnce.Do(func() { close(releaseEvent) })
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
