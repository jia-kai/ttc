package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"ttc/internal/llm"
)

func TestNoToolsCompactionPreservesCachePrefix(t *testing.T) {
	var bodies []map[string]any
	var sessionIDs []string
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		bodies = append(bodies, body)
		sessionIDs = append(sessionIDs, r.Header.Get("session-id"))
		fmt.Fprint(w, sseFrames(map[string]any{"type": "response.completed"}))
	})
	req := llm.Request{
		ConversationID: "session_cache",
		Selection:      llm.Selection{Provider: "openai", Model: llm.ScriptModel()},
		System:         "Stable coding instructions",
		Messages: []llm.Message{
			{Type: llm.TypeUserInput, Content: "Read the notes"},
			{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "read_notes", Name: "read", Arguments: json.RawMessage(`{"path":"notes.txt"}`)}}},
			{Type: llm.TypeToolResult, CallID: "read_notes", Content: "Research notes"},
		},
		Tools: []llm.ToolDefinition{
			{Name: "read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)},
			{Name: "shell", Description: "Run a command", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"timeout_ms":{"type":"integer","minimum":1}},"required":["command"]}`)},
		},
		MaxAttempts: 1,
	}
	if err := a.Stream(context.Background(), req, func(llm.StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	req.NoTools = true
	req.Messages = append(req.Messages, llm.Message{Type: llm.TypeRuntimeInstruction, Content: "Return a structured compaction summary"})
	if err := a.Stream(context.Background(), req, func(llm.StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("got %d requests, want coding and compaction", len(bodies))
	}
	coding, compaction := bodies[0], bodies[1]
	for i, body := range bodies {
		if sessionIDs[i] != req.ConversationID || body["prompt_cache_key"] != req.ConversationID || body["instructions"] != req.System || body["store"] != false {
			t.Errorf("request %d lost cache identity, instructions, or storage policy: %v", i, body)
		}
		if _, chained := body["previous_response_id"]; chained {
			t.Error("server response chaining enabled")
		}
	}
	var wantTools any
	if err := json.Unmarshal([]byte(`[
		{"type":"function","name":"read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false},"strict":false},
		{"type":"function","name":"shell","description":"Run a command","parameters":{"type":"object","properties":{"command":{"type":"string"},"timeout_ms":{"type":"integer","minimum":1}},"required":["command"]},"strict":false}
	]`), &wantTools); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(coding["tools"], wantTools) || !reflect.DeepEqual(compaction["tools"], coding["tools"]) {
		t.Fatalf("tool schemas changed: coding=%v compaction=%v", coding["tools"], compaction["tools"])
	}
	if coding["tool_choice"] != "auto" || compaction["tool_choice"] != "none" {
		t.Fatal("incorrect tool choice", coding["tool_choice"], compaction["tool_choice"])
	}
	codingInput, codingOK := coding["input"].([]any)
	compactionInput, compactionOK := compaction["input"].([]any)
	if !codingOK || !compactionOK || len(codingInput) != 3 || len(compactionInput) != len(codingInput)+1 || !reflect.DeepEqual(compactionInput[:len(codingInput)], codingInput) {
		t.Fatal("compaction changed the history prefix", coding["input"], compaction["input"])
	}
	// Only the appended directive and disabled tool choice should differ.
	compaction["input"] = coding["input"]
	compaction["tool_choice"] = coding["tool_choice"]
	if !reflect.DeepEqual(compaction, coding) {
		t.Fatal("compaction changed other request fields", coding, compaction)
	}
}

func TestNoToolsNamingWithoutSchemasKeepsEmptyTools(t *testing.T) {
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["tools"]) != "[]" || string(body["tool_choice"]) != `"none"` {
			t.Error("nil-tools naming request changed", string(body["tools"]), string(body["tool_choice"]))
		}
		fmt.Fprint(w, sseFrames(map[string]any{"type": "response.completed"}))
	})
	err := a.Stream(context.Background(), llm.Request{ConversationID: "naming", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, NoTools: true, MaxAttempts: 1}, func(llm.StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestCacheAffinityIsRequestScopedAndKeepsStorageDisabled(t *testing.T) {
	seen := make(chan string, 4)
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		key, _ := body["prompt_cache_key"].(string)
		if key == "" || r.Header.Get("session-id") != key || body["store"] != false || body["instructions"] != "Stable" {
			t.Error("incorrect affinity/storage", key, r.Header.Get("session-id"), body["store"])
		}
		if _, chained := body["previous_response_id"]; chained {
			t.Error("server response chaining enabled")
		}
		seen <- key
		fmt.Fprint(w, sseFrames(map[string]any{"type": "response.completed", "response": map[string]any{"usage": map[string]any{
			"input_tokens": 1200, "output_tokens": 5, "input_tokens_details": map[string]any{"cached_tokens": 1024},
		}}}))
	})
	var wg sync.WaitGroup
	for _, id := range []string{"session_a", "main/child_b"} {
		wg.Go(func() {
			for range 2 {
				err := a.Stream(context.Background(), llm.Request{ConversationID: id, Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, System: "Stable", MaxAttempts: 1}, func(ev llm.StreamEvent) error {
					if ev.Kind == "completed" && (ev.Usage == nil || ev.Usage.CachedInputTokens == nil || *ev.Usage.CachedInputTokens != 1024) {
						t.Error("lost cached input counter", ev.Usage)
					}
					return nil
				})
				if err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	close(seen)
	counts := map[string]int{}
	for id := range seen {
		counts[id]++
	}
	if counts["session_a"] != 2 || counts["main/child_b"] != 2 || len(counts) != 2 {
		t.Fatal(counts)
	}
}

func TestCacheAffinityRejectsInvalidIdentityBeforeNetwork(t *testing.T) {
	a := adapterFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid identity reached transport") })
	for _, id := range []string{"", " ", "two words", "bad\r\nheader", "bad\tkey", "bad\x00key", "bad\x7fkey", "非ASCII"} {
		err := a.Stream(context.Background(), llm.Request{ConversationID: id, Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}}, func(llm.StreamEvent) error {
			t.Error("invalid identity emitted output")
			return nil
		})
		if err == nil {
			t.Errorf("accepted invalid identity %q", id)
		}
	}
}
