package session

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestRuntimeInstructionEventsCarryCanonicalIdentity(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Inspect instruction provenance")
	var mu sync.Mutex
	var events []Event
	r.Emit = func(event Event) {
		if event.Kind == "message" && event.MessageType == llm.TypeRuntimeInstruction {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		}
	}
	warning := llm.Message{Type: llm.TypeRuntimeInstruction, Content: "Recovery warning"}
	if added, err := r.ensureRecovery("", "main", warning); err != nil || !added {
		t.Fatal("recovery instruction not committed", added, err)
	}
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		return emit(llm.StreamEvent{Kind: "text", Text: "Aside evidence"})
	}}
	id, err := r.StartBTW("Explain this separately")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := r.Jobs.Wait(ctx, "main", id, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("recovery and aside must both emit typed instructions: %+v", events)
	}
	for _, event := range events {
		entry, err := r.Store.Entry(event.EntryID)
		if err != nil {
			t.Fatal(err)
		}
		var message llm.Message
		if err := json.Unmarshal(entry.Content, &message); err != nil {
			t.Fatal(err)
		}
		if message.Type != event.MessageType || r.Store.Label(entry) != "Runtime instructions · inspect" {
			t.Fatalf("live identity differs from replay: %+v / %+v", event, message)
		}
	}
}
