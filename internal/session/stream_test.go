package session

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
	"ttc/internal/messagestream"
	"ttc/internal/providers/openai"
)

func observeBoundaryFixture(req llm.Request, event llm.BoundaryEvent) error {
	document, err := new(openai.Adapter).InspectBoundary(event.Direction, event.Payload)
	if err != nil {
		return err
	}
	event.Inspection = &document
	return req.Observe(event)
}

func TestProviderBoundaryStreamLiveReplayAndContextIsolation(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	const capturedOnly = "provider-only inspection evidence"
	calls := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		calls++
		if req.Observe == nil {
			t.Fatal("runtime did not attach provider-boundary observer")
		}
		body, err := json.Marshal(map[string]any{
			"model": "fixture", "instructions": capturedOnly,
			"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "original request"}}}},
			"tools": []any{},
		})
		if err != nil {
			return err
		}
		if err := observeBoundaryFixture(req, llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: body}); err != nil {
			return err
		}
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Done."}); err != nil {
			return err
		}
		if err := observeBoundaryFixture(req, llm.BoundaryEvent{Attempt: 1, Direction: "response", Payload: json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}`)}); err != nil {
			return err
		}
		return observeBoundaryFixture(req, llm.BoundaryEvent{Attempt: 1, Direction: "outcome", Payload: json.RawMessage(`{"status":"success","category":"completed"}`)})
	}}
	input := llm.Message{Type: llm.TypeUserInput, Content: "original request"}
	if err := r.Run(&input); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("provider called %d times", calls)
	}
	var live []messagestream.Item
	positions := map[int64]int{}
	for len(events) > 0 {
		event := <-events
		if event.Kind != "stream_item" {
			continue
		}
		if event.StreamItem == nil || event.StreamItem.ID != event.EntryID || event.SessionID != r.Current() {
			t.Fatalf("missing stream identity: %+v", event)
		}
		if index, found := positions[event.StreamItem.ID]; found {
			live[index] = *event.StreamItem
		} else {
			positions[event.StreamItem.ID] = len(live)
			live = append(live, *event.StreamItem)
		}
	}
	replayed, err := r.Store.StreamItems(context.Background(), r.Current())
	if err != nil || len(live) == 0 || !reflect.DeepEqual(live, replayed) {
		t.Fatalf("live/replay differ: live=%+v replay=%+v err=%v", live, replayed, err)
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if strings.Contains(message.Content, capturedOnly) {
			t.Fatal("inspection capture entered canonical model context")
		}
	}
	found := false
	for _, item := range live {
		inspection, err := r.Store.ReadInspection(context.Background(), item.ID)
		if err != nil {
			t.Fatal(err)
		}
		found = found || strings.Contains(inspection.Text, capturedOnly)
	}
	if !found {
		t.Fatal("captured provider instructions were not inspectable")
	}
}

func TestProviderBoundaryObservationFollowsWritableContinuation(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	seedRuntime(t, r, "Original task")
	actor := "main/child_boundary"
	request, err := r.Store.StartRequest(r.Current(), "", actor, "coding", r.CurrentSelection())
	if err != nil {
		t.Fatal(err)
	}
	started, resume := make(chan struct{}), make(chan struct{})
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, _ func(llm.StreamEvent) error) error {
		if err := observeBoundaryFixture(req, llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: json.RawMessage(`{"model":"fixture","input":[]}`)}); err != nil {
			return err
		}
		close(started)
		<-resume
		return observeBoundaryFixture(req, llm.BoundaryEvent{Attempt: 1, Direction: "outcome", Payload: json.RawMessage(`{"status":"success","category":"completed"}`)})
	}}
	done := make(chan error, 1)
	go func() {
		done <- r.stream(context.Background(), "", actor, request, llm.Request{ConversationID: actor}, func(llm.StreamEvent) error { return nil })
	}()
	<-started
	r.routeMu.Lock()
	before, err := r.Store.Branch(r.Current(), 0)
	if err != nil {
		r.routeMu.Unlock()
		close(resume)
		<-done
		t.Fatal(err)
	}
	cut := before[len(before)-1].ID
	continued, err := r.Store.Continue(r.Current(), llm.NewCompactionContext("Earlier work"), cut, cut+1, nil, time.Now(), nil)
	if err == nil {
		r.mu.Lock()
		r.current = continued.ID
		r.mu.Unlock()
	}
	r.routeMu.Unlock()
	close(resume)
	if streamErr := <-done; err != nil || streamErr != nil {
		t.Fatal(err, streamErr)
	}
	found := false
	for len(events) > 0 {
		event := <-events
		if event.Kind != "stream_item" || event.SessionID != continued.ID {
			continue
		}
		entry, err := r.Store.Entry(event.EntryID)
		if err != nil || entry.SessionID != continued.ID || event.RequestID != request || event.Actor != actor {
			t.Fatalf("continuation routing lost: event=%+v entry=%+v err=%v", event, entry, err)
		}
		found = true
	}
	if !found {
		t.Fatal("no boundary observation was routed to the continuation")
	}
}

func TestBoundaryPublicationDoesNotHoldFrontendStateLocks(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Publication liveness")
	request, err := r.Store.StartRequest(r.Current(), "", "main", "coding", r.CurrentSelection())
	if err != nil {
		t.Fatal(err)
	}
	sink := make(chan Event, 1)
	bailout, stop := make(chan struct{}), make(chan struct{})
	r.Emit = func(event Event) {
		select {
		case sink <- event:
		case <-bailout:
		}
	}
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			select {
			case <-sink:
				// Drawing reads this state between draining individual events.
				r.SteeringPreview(3)
			case <-stop:
				return
			}
		}
	}()
	input := make([]any, 20)
	for i := range input {
		input[i] = map[string]any{"type": "message", "role": "user", "content": i}
	}
	payload, err := json.Marshal(map[string]any{"input": input})
	if err != nil {
		t.Fatal(err)
	}
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, _ func(llm.StreamEvent) error) error {
		return observeBoundaryFixture(req, llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: payload})
	}}
	done := make(chan error, 1)
	go func() {
		done <- r.stream(context.Background(), "", "main", request, llm.Request{ConversationID: r.Current()}, func(llm.StreamEvent) error { return nil })
	}()
	select {
	case err := <-done:
		close(stop)
		<-consumerDone
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(bailout)
		<-done
		close(stop)
		<-consumerDone
		t.Fatal("boundary publication blocked frontend state reads")
	}
}
