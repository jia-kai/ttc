package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"ttc/internal/llm"
)

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
