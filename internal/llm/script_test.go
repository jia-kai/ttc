package llm

import (
	"context"
	"testing"
)

func TestScriptDoesNotFabricateProviderUsage(t *testing.T) {
	script := &Script{Responses: []ScriptResponse{{Text: "Offline answer."}}}
	completed := false
	err := script.Stream(context.Background(), Request{System: "Instructions"}, func(event StreamEvent) error {
		if event.Kind == "completed" {
			completed = true
			if event.Usage != nil {
				t.Fatal("offline provider fabricated usage", event.Usage)
			}
		}
		return nil
	})
	if err != nil || !completed {
		t.Fatal("missing completed offline response", err)
	}
}
