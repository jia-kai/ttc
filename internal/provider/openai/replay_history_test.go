package openai

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/history"
	"ttc/internal/provider"
)

func TestNativeToolReplayAfterHistorySerialization(t *testing.T) {
	selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
	store, err := history.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := history.NewID("session")
	turn, _, err := store.StartSession(id, t.TempDir(), selection, provider.Message{Role: "user", Content: "Read the research notes"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := store.StartRequest(id, turn, "main", "coding", selection)
	if err != nil {
		t.Fatal(err)
	}
	arguments := json.RawMessage("{\n  \"path\": \"<research>&notes.txt\",\n  \"nested\": { \"large\": 9007199254740993 }\n}")
	native, err := json.Marshal(map[string]any{"type": "function_call", "id": "item", "call_id": "provider-call", "name": "read", "arguments": string(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	m := provider.Message{Role: "assistant", Calls: []provider.ToolCall{{ID: "provider-call", Name: "read", Arguments: arguments}}}
	if err = m.AppendState(selection, replayVersion, native); err != nil {
		t.Fatal(err)
	}
	if _, err = wire(context.Background(), provider.Request{ConversationID: "test-conversation", Selection: selection, Messages: []provider.Message{m}}); err != nil {
		t.Fatal("initial replay", err)
	}
	if _, _, err = store.Assistant(id, turn, "main", request, m); err != nil {
		t.Fatal(err)
	}
	messages, err := store.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wire(context.Background(), provider.Request{ConversationID: "test-conversation", Selection: selection, Messages: messages}); err != nil {
		t.Fatal("saved replay", err)
	}
	if len(messages) != 2 || messages[0].Role != "user" || messages[1].Role != "assistant" {
		t.Fatalf("unexpected replay history: %+v", messages)
	}
	for _, field := range []string{"id", "name", "arguments", "large_integer"} {
		changed := messages[1]
		changed.Calls = append([]provider.ToolCall(nil), changed.Calls...)
		switch field {
		case "id":
			changed.Calls[0].ID = "different"
		case "name":
			changed.Calls[0].Name = "different"
		case "large_integer":
			changed.Calls[0].Arguments = json.RawMessage(strings.Replace(string(arguments), "9007199254740993", "9007199254740992", 1))
		case "arguments":
			changed.Calls[0].Arguments = json.RawMessage(`{"path":"changed"}`)
		}
		if _, err = wire(context.Background(), provider.Request{ConversationID: "test-conversation", Selection: selection, Messages: []provider.Message{changed}}); err == nil {
			t.Fatal("changed canonical", field, "accepted")
		}
	}
}
