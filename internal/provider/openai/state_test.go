package openai

import (
	"context"
	"encoding/json"
	"scicode/internal/provider"
	"strings"
	"testing"
)

func TestNativePhaseStateUsageAndStatelessWire(t *testing.T) {
	reason := map[string]any{"type": "reasoning", "id": "rs", "encrypted_content": "encrypted"}
	message := map[string]any{"type": "message", "id": "msg", "role": "assistant", "status": "completed", "phase": "commentary", "content": []any{map[string]any{"type": "output_text", "text": "Working on it.", "annotations": []any{}}}}
	events := []any{
		map[string]any{"type": "response.output_text.delta", "delta": "Working on it."},
		map[string]any{"type": "response.output_item.done", "output_index": 1, "item": message},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": reason},
		map[string]any{"type": "response.completed", "response": map[string]any{"usage": map[string]any{"input_tokens": 1000, "output_tokens": 100, "input_tokens_details": map[string]any{"cached_tokens": 800, "cache_write_tokens": 100}, "output_tokens_details": map[string]any{"reasoning_tokens": 70}}}},
	}
	selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
	reply := provider.Message{Role: "assistant"}
	var usage *provider.Usage
	_, err := parseStream(strings.NewReader(sseFrames(events...)), func(e provider.StreamEvent) error {
		switch e.Kind {
		case "text":
			reply.Content += e.Text
		case "phase":
			reply.Phase = e.Phase
		case "state":
			return reply.AppendState(selection, e.StateVersion, e.StateItem)
		case "completed":
			usage = e.Usage
		}
		return nil
	})
	if err != nil || reply.Phase != "commentary" || reply.State == nil || len(reply.State.Items) != 2 || !strings.Contains(string(reply.State.Items[0]), "encrypted_content") {
		t.Fatal(reply, err)
	}
	if usage == nil || usage.CachedInputTokens == nil || *usage.CachedInputTokens != 800 || usage.CacheWriteTokens == nil || *usage.CacheWriteTokens != 100 || usage.ReasoningOutputTokens == nil || *usage.ReasoningOutputTokens != 70 {
		t.Fatal(usage)
	}
	body, err := wire(provider.Request{ConversationID: "test-conversation", Selection: selection, System: "Stable", Messages: []provider.Message{reply}})
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Store      bool
		Input      []map[string]any
		PreviousID string `json:"previous_response_id"`
	}
	if err = json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if request.Store || request.PreviousID != "" || len(request.Input) != 2 || request.Input[1]["phase"] != "commentary" {
		t.Fatal(string(body))
	}
	reply.State = nil
	body, err = wire(provider.Request{ConversationID: "test-conversation", Selection: selection, Messages: []provider.Message{reply}})
	if err != nil || !strings.Contains(string(body), `"phase":"commentary"`) {
		t.Fatal(string(body), err)
	}
}

func TestStateIsolationCodecAndCanonicalConsistency(t *testing.T) {
	selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
	item := json.RawMessage(`{"type":"reasoning","encrypted_content":"private"}`)
	for _, test := range []struct {
		name, provider, model string
		version               int
		wantError             bool
	}{
		{"matching", "openai", "scripted", 1, false},
		{"foreign-provider", "other", "scripted", 999, false},
		{"foreign-model", "openai", "other", 999, false},
		{"unsupported-codec", "openai", "scripted", 2, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := provider.Message{Role: "assistant", State: &provider.ReplayState{Provider: test.provider, Model: test.model, Version: test.version, Items: []json.RawMessage{item}}}
			body, err := wire(provider.Request{ConversationID: "test-conversation", Selection: selection, Messages: []provider.Message{m}})
			if (err != nil) != test.wantError {
				t.Fatal(err)
			}
			if !test.wantError && strings.Contains(string(body), "private") != (test.name == "matching") {
				t.Fatal(string(body))
			}
			if m.State.Provider != test.provider || string(m.State.Items[0]) != string(item) {
				t.Fatal("mutated saved state")
			}
		})
	}
	m := provider.Message{Role: "assistant", Content: "different", State: &provider.ReplayState{Provider: "openai", Model: "scripted", Version: 1, Items: []json.RawMessage{item}}}
	if _, err := wire(provider.Request{ConversationID: "test-conversation", Selection: selection, Messages: []provider.Message{m}}); err == nil {
		t.Fatal("accepted contradictory canonical state")
	}
}

func TestWrongProviderRejectedBeforeAuthAndNetwork(t *testing.T) {
	a := New("not-a-credential-file")
	err := a.Stream(context.Background(), provider.Request{ConversationID: "test-conversation", Selection: provider.Selection{Provider: "other", Model: provider.ScriptModel()}}, func(provider.StreamEvent) error { t.Fatal("unexpected output"); return nil })
	if err == nil || !strings.Contains(err.Error(), "OpenAI model selection") {
		t.Fatal(err)
	}
}

func messageDone(index int, text string) map[string]any {
	return map[string]any{"type": "response.output_item.done", "output_index": index, "item": map[string]any{"type": "message", "id": "msg", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}}
}

func TestRefusalAndContradictoryNativeMessage(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		message := messageDone(0, "Declined")
		message["item"].(map[string]any)["content"] = []any{map[string]any{"type": "refusal", "refusal": "Declined"}}
		delta := "Declined"
		if mismatch {
			delta = "Other"
		}
		m := provider.Message{Role: "assistant"}
		selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
		completed := false
		_, err := parseStream(strings.NewReader(sseFrames(map[string]any{"type": "response.refusal.delta", "delta": delta}, message, map[string]any{"type": "response.completed"})), func(e provider.StreamEvent) error {
			switch e.Kind {
			case "text":
				m.Content += e.Text
			case "state":
				return m.AppendState(selection, e.StateVersion, e.StateItem)
			case "completed":
				completed = true
			}
			return nil
		})
		if mismatch {
			if err == nil || completed {
				t.Fatal("contradictory response accepted")
			}
			continue
		}
		if err != nil || m.Content != "Declined" || !completed {
			t.Fatal(m, err)
		}
		if _, err = wire(provider.Request{ConversationID: "test-conversation", Selection: selection, Messages: []provider.Message{m}}); err != nil {
			t.Fatal(err)
		}
	}
}
