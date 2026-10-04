package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"ttc/internal/provider"
)

func TestExplicitAndSyntheticNoneReasoning(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		model := provider.ScriptModel()
		model.SupportsReasoning = explicit
		body, err := wire(provider.Request{ConversationID: "test-conversation", Selection: provider.Selection{Provider: "openai", Model: model, Variant: "none"}})
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
	a := New(filepath.Join(t.TempDir(), "private", "auth.json"))
	a.Client = &http.Client{Transport: handlerTransport(h)}
	a.BaseURL = "https://mock.test"
	a.AuthURL = a.BaseURL
	if e := a.save(Credentials{AuthMode: "chatgpt", Tokens: Tokens{Access: "token", AccountID: "account"}, LastRefresh: time.Now()}); e != nil {
		t.Fatal(e)
	}
	return a
}

func TestCatalogAuthenticationFailuresStayLocal(t *testing.T) {
	var requests atomic.Int32
	transport := handlerTransport(func(http.ResponseWriter, *http.Request) { requests.Add(1) })
	dir := t.TempDir()
	a := New(filepath.Join(dir, "missing.json"))
	a.BaseURL = "https://mock.test"
	a.Client = &http.Client{Transport: transport}
	if _, err := a.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "ttc --login") {
		t.Fatalf("missing credentials must offer a usable startup command: %v", err)
	}
	// A broken path must report its filesystem error rather than asking for another login.
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	a.CredentialPath = filepath.Join(file, "auth.json")
	_, err := a.Models(context.Background())
	var pathError *os.PathError
	if !errors.As(err, &pathError) {
		t.Fatalf("credential filesystem failure was hidden: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("catalog request sent without valid credentials")
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
	sel, err := provider.Resolve("openai", models, "first", "")
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
	if e != nil || len(models) != 1 || models[0].Budget.ContextLimit != 95000 || models[0].Budget.MaxOutputTokens != 0 {
		t.Fatal(models, e)
	}
	sel, e := provider.Resolve("openai", models, "test", "low")
	if e != nil {
		t.Fatal(e)
	}
	events := []provider.StreamEvent{}
	e = a.Stream(context.Background(), provider.Request{ConversationID: "test-conversation", Selection: sel, System: "system", Messages: []provider.Message{{Role: "user", Content: "hello"}}}, func(e provider.StreamEvent) error { events = append(events, e); return nil })
	if e != nil || len(events) != 7 || events[5].Call.ID != "c" {
		t.Fatal(events, e)
	}
	b, e := wire(provider.Request{ConversationID: "test-conversation", Selection: sel, Messages: []provider.Message{{Role: "assistant", State: &provider.ReplayState{Provider: "openai", Model: sel.Model.RequestID(), Version: 1, Items: []json.RawMessage{events[2].StateItem}}}}})
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
				e := a.Stream(context.Background(), provider.Request{ConversationID: "test-conversation", Selection: provider.Selection{Provider: "openai", Model: provider.ScriptModel()}}, func(ev provider.StreamEvent) error {
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
func TestExplicitImportRejectsAPIKeysAndPrivateCredentials(t *testing.T) {
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	p := filepath.Join(t.TempDir(), "codex.json")
	os.WriteFile(p, []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"secret"}`), 0600)
	if e := a.ImportCodex(p); e == nil {
		t.Fatal("accepted API billing")
	}
	os.WriteFile(p, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"imported","account_id":"account"}}`), 0600)
	if e := a.ImportCodex(p); e != nil {
		t.Fatal(e)
	}
	st, e := os.Stat(a.CredentialPath)
	if e != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, e)
	}
}

type testLogin struct{ steps []provider.LoginStep }

func (u *testLogin) Present(ctx context.Context, s provider.LoginStep) (provider.LoginAnswer, error) {
	u.steps = append(u.steps, s)
	return provider.LoginAnswer{}, nil
}
func TestDeviceLoginAndCancellation(t *testing.T) {
	claim := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`))
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			fmt.Fprint(w, `{"device_auth_id":"d","user_code":"CODE","interval":"1"}`)
		case "/api/accounts/deviceauth/token":
			fmt.Fprint(w, `{"authorization_code":"authorized","code_verifier":"verify"}`)
		case "/oauth/token":
			if e := r.ParseForm(); e != nil || r.Form.Get("grant_type") != "authorization_code" {
				t.Error("wrong exchange")
			}
			fmt.Fprintf(w, `{"access_token":"a","refresh_token":"r","id_token":"x.%s.y"}`, claim)
		default:
			t.Error(r.URL.Path)
		}
	})
	ui := &testLogin{}
	if e := a.Login(context.Background(), ui); e != nil {
		t.Fatal(e)
	}
	if len(ui.steps) != 2 || ui.steps[0].Code != "CODE" {
		t.Fatal(ui.steps)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := a.Login(ctx, &testLogin{}); e == nil {
		t.Fatal("ignored canceled login")
	}
}

func TestOneAttemptMetadataDoesNotRetryAndBackoffCancels(t *testing.T) {
	var requests atomic.Int32
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	req := provider.Request{ConversationID: "test-conversation", Selection: provider.Selection{Provider: "openai", Model: provider.ScriptModel()}, NoTools: true, MaxAttempts: 1}
	if e := a.Stream(context.Background(), req, func(provider.StreamEvent) error { return nil }); e == nil || requests.Load() != 1 {
		t.Fatal(e, requests.Load())
	}
	req.MaxAttempts = 0
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if e := a.Stream(ctx, req, func(provider.StreamEvent) error { return nil }); e == nil || requests.Load() != 2 {
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
	if _, err := provider.Resolve("openai", models, "model-b/fast", ""); err == nil {
		t.Fatal("invented unsupported Fast choice")
	}
	for _, id := range []string{"model-a", "model-a/fast"} {
		selection, err := provider.Resolve("openai", models, id, "high")
		if err != nil {
			t.Fatal(err)
		}
		err = a.Stream(context.Background(), provider.Request{ConversationID: "test-conversation", Selection: selection}, func(ev provider.StreamEvent) error {
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
