package session

import (
	"testing"

	"ttc/internal/llm"
)

func TestHumanInputEventsUseCommittedMetadata(t *testing.T) {
	for _, source := range []string{"", "normal", "queue"} {
		t.Run("source="+source, func(t *testing.T) {
			r, events := runtimeFixture(t, []llm.ScriptResponse{{Text: "First reply"}, {Text: "Second reply"}})
			for _, text := range []string{"First input", "Second input"} {
				input := llm.Message{Type: llm.TypeUserInput, Content: text, InputSource: source, InputTimeMS: 1}
				if err := r.Run(&input); err != nil {
					t.Fatal(err)
				}
				found := false
				for len(events) > 0 {
					event := <-events
					if event.MessageType != llm.TypeUserInput {
						if event.InputSource != "" || event.InputTimeMS != 0 {
							t.Fatalf("non-input event has human metadata: %+v", event)
						}
						continue
					}
					found = true
					entry, err := r.Store.Entry(event.EntryID)
					if err != nil {
						t.Fatal(err)
					}
					wantSource := source
					if wantSource == "" {
						wantSource = "normal"
					}
					if event.Text != text || event.InputSource != wantSource || event.InputTimeMS != entry.CreatedMS || event.InputTimeMS <= 1 {
						t.Fatalf("event does not match committed input: %+v, entry time %d", event, entry.CreatedMS)
					}
				}
				if !found {
					t.Fatal("missing human input event")
				}
			}
		})
	}
}
