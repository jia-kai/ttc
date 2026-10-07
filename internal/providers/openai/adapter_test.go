package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"ttc/internal/llm"
)

func TestExplicitAndSyntheticNoneReasoning(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		model := llm.ScriptModel()
		model.SupportsReasoning = explicit
		body, err := wire(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: model, Variant: "none"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		reasoning, present := fields["reasoning"]
		if present != explicit || explicit && !strings.Contains(string(reasoning), `"effort":"none"`) {
			t.Fatal(explicit, string(body))
		}
	}
}

// handlerTransport exercises request headers/status/body and SSE codecs without
// binding sockets. The external mock integration covers real HTTP/PTY transport.
func handlerTransport(h http.HandlerFunc) retryTransport {
	return func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		if r.URL.Scheme != "http" && r.URL.Scheme != "https" {
			return nil, fmt.Errorf("unsupported protocol scheme %q", r.URL.Scheme)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		result := w.Result()
		result.Request = r
		return result, nil
	}
}

func adapterFixture(t *testing.T, h http.HandlerFunc) *Adapter {
	t.Helper()
	return NewAdapter(Config{Client: &http.Client{Transport: handlerTransport(h)}, BaseURL: "https://mock.test", TokenSource: func(ctx context.Context) (AccessTokens, error) {
		return AccessTokens{Access: "token", AccountID: "account"}, ctx.Err()
	}})
}

// testModels applies explicit test-only request policy after metadata discovery.
func testModels(models []llm.ModelInfo) []llm.ModelSpec {
	out := make([]llm.ModelSpec, len(models))
	for i, m := range models {
		budget := llm.ScriptModel().Budget
		budget.ContextLimit = m.Limits.ContextLimit
		budget.MaxOutputTokens = m.Limits.MaxOutputTokens
		out[i] = llm.ModelSpec{ID: m.ID, BaseID: m.BaseID, ServiceTier: m.ServiceTier, Description: m.Description, Name: m.Name, Variants: m.Variants, VariantDescriptions: m.VariantDescriptions, DefaultVariant: m.DefaultVariant, Images: m.Images, BinaryFiles: m.BinaryFiles, SupportsReasoning: m.SupportsReasoning, Budget: budget, Revision: m.Revision}
	}
	return out
}

func TestCatalogMissingCredentialsSourceFailsBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	a := NewAdapter(Config{Client: &http.Client{Transport: handlerTransport(func(http.ResponseWriter, *http.Request) { requests.Add(1) })}})
	if _, err := a.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "credentials source") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Models(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("catalog sent request without credentials", requests.Load())
	}
}

func TestCatalogWithMemoryCredentialsCreatesNoFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); ok {
			t.Error("transport imposed a lifecycle deadline")
		}
		fmt.Fprint(w, `{"models":[{"slug":"tiny","visibility":"list","context_window":100}]}`)
	})
	models, err := a.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].Limits.ContextLimit != 100 {
		t.Fatal(models, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestCatalogPriorityVisibilityAndVariants(t *testing.T) {
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[
			{"slug":"later","display_name":"Later","visibility":"list","priority":20,"context_window":100000,"default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low"}]},
			{"slug":"internal-review","display_name":"Internal","visibility":"hide","priority":0},
			{"slug":"first","display_name":"First","visibility":"list","priority":10,"context_window":100000,"default_reasoning_level":"high","supported_reasoning_levels":[{"effort":"low","description":"Quick"},{"effort":"high","description":"Deeper reasoning"}]}]}`)
	})
	models, err := a.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "first" || models[1].ID != "later" {
		t.Fatalf("picker must preserve provider priority and exclude hidden models: %+v", models)
	}
	for _, model := range models {
		if !reflect.DeepEqual(model.BinaryFiles, documentFileTypes()) {
			t.Fatalf("model did not announce native document formats: %+v", model.BinaryFiles)
		}
	}
	sel, err := llm.Resolve("openai", testModels(models), "first", "")
	if err != nil || sel.Variant != "high" || sel.Model.VariantDescriptions["high"] != "Deeper reasoning" {
		t.Fatal(sel, err)
	}
}

func TestCatalogRejectsUnselectableOrInvalidMetadata(t *testing.T) {
	for _, body := range []string{
		`{"models":[]}`,
		`{"models":[{"slug":"review","visibility":"hide"}]}`,
		`{"models":[{"slug":"bad","visibility":"unknown"}]}`,
		`{"models":[{"slug":"bad","visibility":"list","context_window":100000,"default_reasoning_level":"high","supported_reasoning_levels":[{"effort":"low"}]}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			if models, err := a.Models(context.Background()); err == nil {
				t.Fatalf("invalid catalog accepted: %+v", models)
			}
		})
	}
}
func TestCatalogAndStreamToolReasoningContinuity(t *testing.T) {
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("ChatGPT-Account-ID") != "account" {
			t.Error("missing auth headers")
		}
		if r.URL.Path == "/models" {
			if r.URL.Query().Get("client_version") != "0.159.0" {
				t.Error("catalog uses an obsolete client contract")
			}
			fmt.Fprint(w, `{"models":[{"slug":"test","visibility":"list","display_name":"Test","context_window":100000,"effective_context_window_percent":95,"default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low"}],"input_modalities":["text","image"]}]}`)
			return
		}
		var body map[string]json.RawMessage
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		if _, ok := body["max_output_tokens"]; ok {
			t.Error("invented subscription output cap")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"r\",\"encrypted_content\":\"opaque\"}}\n\n")
		fmt.Fprint(w, sseFrames(messageDone(1, "hello")))
		fmt.Fprint(w, functionFrames(2, "item_c", "c", "read", `{"path":"x"}`))
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"res\",\"usage\":{\"input_tokens\":10,\"output_tokens\":4}}}\n\n")
	})
	models, e := a.Models(context.Background())
	if e != nil || len(models) != 1 || models[0].Limits.ContextLimit != 95000 || models[0].Limits.MaxOutputTokens != 0 {
		t.Fatal(models, e)
	}
	sel, e := llm.Resolve("openai", testModels(models), "test", "low")
	if e != nil {
		t.Fatal(e)
	}
	events := []llm.StreamEvent{}
	e = a.Stream(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: sel, System: "system", Messages: []llm.Message{{Role: "user", Content: "hello"}}}, func(e llm.StreamEvent) error { events = append(events, e); return nil })
	if e != nil || len(events) != 9 || events[7].Call.ID != "c" {
		t.Fatal(events, e)
	}
	var reasoning json.RawMessage
	for _, event := range events {
		if strings.Contains(string(event.StateItem), "encrypted_content") {
			reasoning = event.StateItem
		}
	}
	b, e := wire(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: sel, Messages: []llm.Message{{Role: "assistant", State: &llm.ReplayState{Provider: "openai", Model: sel.Model.RequestID(), Version: 1, Items: []json.RawMessage{reasoning}}}}}, nil)
	if e != nil || !strings.Contains(string(b), "encrypted_content") {
		t.Fatal(string(b), e)
	}
}
func TestPartialOutputIsNeverRetried(t *testing.T) {
	for _, tc := range []struct{ kind, event string }{
		{"text", `{"type":"response.output_text.delta","delta":"partial"}`},
		{"call_start", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item_c","call_id":"c","name":"read","arguments":""}}`},
	} {
		for _, callbackFailure := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/callbackFailure=%t", tc.kind, callbackFailure), func(t *testing.T) {
				var requests atomic.Int32
				a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					fmt.Fprint(w, "data: "+tc.event+"\n\n")
				})
				failure := errors.New("consumer failure")
				e := a.Stream(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}}, func(ev llm.StreamEvent) error {
					if ev.Kind != tc.kind {
						t.Errorf("unexpected event %s", ev.Kind)
					}
					if callbackFailure {
						return failure
					}
					return nil
				})
				if e == nil || requests.Load() != 1 || callbackFailure && !errors.Is(e, failure) {
					t.Fatal(e, requests.Load())
				}
			})
		}
	}
}
func TestOneAttemptMetadataDoesNotRetryAndBackoffCancels(t *testing.T) {
	var requests atomic.Int32
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	req := llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, NoTools: true, MaxAttempts: 1}
	if e := a.Stream(context.Background(), req, func(llm.StreamEvent) error { return nil }); e == nil || requests.Load() != 1 {
		t.Fatal(e, requests.Load())
	}
	req.MaxAttempts = 0
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if e := a.Stream(ctx, req, func(llm.StreamEvent) error { return nil }); e == nil || requests.Load() != 2 {
		t.Fatal(e, requests.Load())
	}
}

func TestCatalogFastChoiceAndTransport(t *testing.T) {
	requests := make(chan map[string]any, 2)
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"models":[{"slug":"model-a","display_name":"Model A","visibility":"list","context_window":100000,"default_reasoning_level":"high","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}],"service_tiers":[{"id":"priority","name":"Fast","description":"Increased usage"}]},{"slug":"model-b","display_name":"Model B","visibility":"list","context_window":100000,"default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low"}]}]}`)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests <- body
		want := "model=model-a;tier=" + body["service_tier"].(string)
		if r.Header.Get("x-codex-routing-hint") != want {
			t.Error(r.Header.Get("x-codex-routing-hint"), want)
		}
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"done\",\"service_tier\":\"default\"}}\n\n")
	})
	models, err := a.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 || models[0].Name != "Model A (Standard)" || models[1].ID != "model-a/fast" || models[1].Description != "Increased usage" || models[2].ServiceTier != "" {
		t.Fatal(models)
	}
	if _, err := llm.Resolve("openai", testModels(models), "model-b/fast", ""); err == nil {
		t.Fatal("invented unsupported Fast choice")
	}
	for _, id := range []string{"model-a", "model-a/fast"} {
		selection, err := llm.Resolve("openai", testModels(models), id, "high")
		if err != nil {
			t.Fatal(err)
		}
		err = a.Stream(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: selection}, func(ev llm.StreamEvent) error {
			if ev.Kind == "completed" && ev.ServiceTier != "default" {
				t.Error("actual tier lost", ev)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	bodies := []map[string]any{<-requests, <-requests}
	if bodies[0]["service_tier"] != "default" || bodies[1]["service_tier"] != "priority" {
		t.Fatal(bodies)
	}
	for _, body := range bodies {
		if body["model"] != "model-a" || body["reasoning"].(map[string]any)["effort"] != "high" {
			t.Fatal(body)
		}
	}
}
