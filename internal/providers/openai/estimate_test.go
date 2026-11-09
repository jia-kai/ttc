package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"ttc/internal/llm"
)

func tokenRequest(texts ...string) llm.Request {
	req := llm.Request{ConversationID: "estimate", Selection: llm.Selection{Provider: "openai", Model: llm.ModelSpec{ID: "gpt-6-sol"}}, System: "instructions"}
	for _, text := range texts {
		req.Messages = append(req.Messages, llm.Message{Type: llm.TypeUserInput, Content: text})
	}
	return req
}

func estimated(t *testing.T, a *Adapter, req llm.Request) llm.InputEstimate {
	t.Helper()
	result, err := a.EstimateInput(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Checkpoint == nil || result.Checkpoint.Version != 2 || result.Checkpoint.Validate() != nil {
		t.Fatalf("invalid native checkpoint: %+v", result)
	}
	return result
}

func rememberEstimate(t *testing.T, a *Adapter, result llm.InputEstimate, input int) {
	t.Helper()
	if err := a.RestoreEstimate(*result.Checkpoint, llm.Usage{InputTokens: input}); err != nil {
		t.Fatal(err)
	}
}

func TestStrictMeasuredPrefix(t *testing.T) {
	a := &Adapter{}
	first := estimated(t, a, tokenRequest("one"))
	long := estimated(t, a, tokenRequest("one", "two", "three"))
	if !first.Estimated || !long.Estimated {
		t.Fatal("estimate learned without usage")
	}
	rememberEstimate(t, a, first, 100)
	if got := estimated(t, a, tokenRequest("one")); got.Input != 100 || got.Estimated {
		t.Fatalf("exact measurement lost: %+v", got)
	}
	if got := estimated(t, a, tokenRequest("one", "two", "three")); got.Input != 100+long.Input-first.Input || !got.Estimated {
		t.Fatalf("measured prefix not substituted: %+v", got)
	}
	rememberEstimate(t, a, long, 300)
	// A complete long request did not measure its interior prefixes.
	if got := estimated(t, a, tokenRequest("one", "two")); got.Input == 300 || !got.Estimated {
		t.Fatalf("interior prefix incorrectly measured: %+v", got)
	}
	// Neither a longer observation nor a divergent sibling authorizes a signed delta.
	b := &Adapter{}
	rememberEstimate(t, b, long, 300)
	if got := estimated(t, b, tokenRequest("one")); got.Input != first.Input || !got.Estimated {
		t.Fatalf("short input borrowed longer measurement: %+v", got)
	}
	sibling := tokenRequest("different", "two", "three")
	if got := estimated(t, b, sibling); got.Input != estimated(t, &Adapter{}, sibling).Input || !got.Estimated {
		t.Fatalf("divergent input borrowed measurement: %+v", got)
	}
}

func TestMeasuredPrefixLongestAndDivergence(t *testing.T) {
	a := &Adapter{}
	one := estimated(t, a, tokenRequest("one"))
	two := estimated(t, a, tokenRequest("one", "two"))
	three := estimated(t, a, tokenRequest("one", "two", "three"))
	rememberEstimate(t, a, one, 100)
	rememberEstimate(t, a, two, 200)
	if got := estimated(t, a, tokenRequest("one", "two", "three")); got.Input != 200+three.Input-two.Input || !got.Estimated {
		t.Fatalf("did not choose longest measured prefix: %+v", got)
	}
	divergent := tokenRequest("one", "changed")
	local := estimated(t, &Adapter{}, divergent)
	if got := estimated(t, a, divergent); got.Input != 100+local.Input-one.Input || !got.Estimated {
		t.Fatalf("did not retain actual shared measured prefix: %+v", got)
	}
}

func TestEstimateConfigurationIsolation(t *testing.T) {
	base := tokenRequest("one")
	base.Tools = []llm.ToolDefinition{{Name: "read", Description: "read a file", Parameters: json.RawMessage(`{"type":"object"}`)}}
	a := &Adapter{}
	first := estimated(t, a, base)
	rememberEstimate(t, a, first, 1000)
	for _, tc := range []struct {
		name string
		edit func(*llm.Request)
	}{
		{"model", func(r *llm.Request) { r.Selection.Model.ID = "gpt-6-luna" }},
		{"system", func(r *llm.Request) { r.System += " changed" }},
		{"tools", func(r *llm.Request) { r.Tools = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.edit(&req)
			got := estimated(t, a, req)
			if !got.Estimated || got.Checkpoint.Compatible == first.Checkpoint.Compatible {
				t.Fatalf("configuration reused measured input: %+v", got)
			}
		})
	}
	for _, edit := range []func(*llm.Request){
		func(r *llm.Request) { r.NoTools = true },
		func(r *llm.Request) { r.Selection.Model.ServiceTier = "priority" },
		func(r *llm.Request) { r.Selection.Variant = "high" },
		func(r *llm.Request) { r.ConversationID = "other-actor" },
		func(r *llm.Request) { r.Selection.Model.ID = "speed-picker"; r.Selection.Model.BaseID = "gpt-6-sol" },
	} {
		req := base
		edit(&req)
		if got := estimated(t, a, req); got.Input != 1000 || got.Estimated {
			t.Fatalf("token-neutral configuration invalidated measurement: %+v", got)
		}
	}
}

func TestEstimateRestoreValidationAndLegacy(t *testing.T) {
	a := &Adapter{}
	req := tokenRequest("one")
	base := estimated(t, a, req)
	legacy := *base.Checkpoint
	legacy.Version = 1
	if err := a.RestoreEstimate(legacy, llm.Usage{InputTokens: 1000}); err != nil {
		t.Fatal(err)
	}
	if got := estimated(t, a, req); !got.Estimated || got.Input != base.Input {
		t.Fatalf("legacy checkpoint seeded new cache: %+v", got)
	}
	for _, edit := range []func(*llm.TokenCheckpoint){
		func(c *llm.TokenCheckpoint) { c.Version = 3 },
		func(c *llm.TokenCheckpoint) { c.LocalInput = -1 },
		func(c *llm.TokenCheckpoint) { c.Compatible = "bad" },
		func(c *llm.TokenCheckpoint) { c.Exact = strings.Repeat("z", 64) },
	} {
		checkpoint := *base.Checkpoint
		edit(&checkpoint)
		if err := a.RestoreEstimate(checkpoint, llm.Usage{InputTokens: 1000}); err == nil {
			t.Fatal("accepted invalid checkpoint", checkpoint)
		}
	}
	for _, usage := range []llm.Usage{{InputTokens: -1}, {OutputTokens: -1}} {
		if err := a.RestoreEstimate(*base.Checkpoint, usage); err == nil {
			t.Fatal("accepted unusable usage", usage)
		}
	}
	mismatch := *base.Checkpoint
	mismatch.LocalInput++
	if err := a.RestoreEstimate(mismatch, llm.Usage{InputTokens: 1000}); err != nil {
		t.Fatal(err)
	}
	if got := estimated(t, a, req); !got.Estimated || got.Input != base.Input {
		t.Fatalf("mismatched local count calibrated input: %+v", got)
	}
	rememberEstimate(t, a, base, 0)
	if got := estimated(t, a, req); got.Input != 0 || got.Estimated {
		t.Fatalf("zero usage was not measured: %+v", got)
	}
}

func TestEstimateCacheBoundedConcurrent(t *testing.T) {
	a := &Adapter{}
	first := estimated(t, a, tokenRequest("first"))
	rememberEstimate(t, a, first, 10)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < measuredInputLimit; i++ {
				req := tokenRequest(fmt.Sprintf("worker %d item %d", worker, i))
				result, err := a.EstimateInput(context.Background(), req)
				if err == nil {
					err = a.RestoreEstimate(*result.Checkpoint, llm.Usage{InputTokens: i})
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(a.estimates.entries) != measuredInputLimit || a.estimates.recent.Len() != measuredInputLimit {
		t.Fatal("cache exceeded entry bound", len(a.estimates.entries), a.estimates.recent.Len())
	}
	if got := estimated(t, a, tokenRequest("first")); !got.Estimated {
		t.Fatal("oldest measurement was not evicted")
	}
}

func TestNativeReplayEstimateExcludesTransportAndBase64Expansion(t *testing.T) {
	item := map[string]any{"type": "reasoning", "id": "transport", "encrypted_content": strings.Repeat("A", 100000), "summary": []any{map[string]any{"text": strings.Repeat("duplicate summary ", 500)}}}
	if got, err := nativeItemTokens(item); err != nil || got != 18588 {
		t.Fatal("transport bytes were treated as model tokens", got, err)
	}
	for _, tc := range []struct {
		item map[string]any
		want int
	}{
		{map[string]any{"type": "reasoning", "encrypted_content": "tiny"}, 0},
		{map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "summary only"}}}, 4},
	} {
		if got, err := nativeItemTokens(tc.item); err != nil || got != tc.want {
			t.Fatal("incorrect reasoning occupancy", got, err)
		}
	}
}

func TestEstimateNativeBinaryOccupancy(t *testing.T) {
	for _, mime := range []string{"image/png", "application/pdf"} {
		t.Run(mime, func(t *testing.T) {
			req := tokenRequest("one")
			req.Selection.Model.Images = true
			req.Selection.Model.BinaryFiles = []llm.BinaryFileType{{MIMEType: mime, Kind: "document", MaxBytes: 1000000, Extensions: []string{"pdf"}}}
			file := llm.BinaryFile{Path: "/original.pdf", MIMEType: mime, Bytes: 90000}
			req.Messages[0].Files = []llm.BinaryFile{file}
			a := NewAdapter(Config{ResolveBinary: func(context.Context, llm.BinaryFile) (llm.BinaryPayload, error) {
				return llm.BinaryPayload{MIMEType: mime, Data: make([]byte, file.Bytes)}, nil
			}})
			got := estimated(t, a, req)
			req.Messages[0].Files = nil
			base := estimated(t, a, req)
			if got.Input-base.Input != file.EstimatedTokens() {
				t.Fatalf("binary transport counted instead of native occupancy: %d != %d", got.Input-base.Input, file.EstimatedTokens())
			}
		})
	}
}

func TestEstimateErrorsAndMutableInputs(t *testing.T) {
	a := &Adapter{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := a.EstimateInput(ctx, tokenRequest("one")); !errors.Is(err, context.Canceled) || result.Input != -1 || result.Checkpoint != nil {
		t.Fatal("canceled estimate returned usable input", result, err)
	}
	if result, err := a.EstimateInput(context.Background(), llm.Request{}); err == nil || result.Input != -1 || result.Checkpoint != nil {
		t.Fatal("invalid estimate returned usable input", result, err)
	}
	req := tokenRequest("one")
	first := estimated(t, a, req)
	rememberEstimate(t, a, first, 1000)
	req.Messages[0].Content = "changed"
	if got := estimated(t, a, req); !got.Estimated || got.Checkpoint.Exact == first.Checkpoint.Exact {
		t.Fatal("mutable input reused cached projection", got)
	}
}

func TestEstimateOverflowReturnsInvalidResult(t *testing.T) {
	a := &Adapter{}
	first := estimated(t, a, tokenRequest("one"))
	rememberEstimate(t, a, first, int(^uint(0)>>1))
	result, err := a.EstimateInput(context.Background(), tokenRequest("one", "two"))
	if err == nil || result.Input != -1 || !result.Estimated || result.Checkpoint != nil {
		t.Fatal("overflow returned usable estimate", result, err)
	}
}

func TestStreamLearnsOnlySuccessfulOwnRequest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		usage    string
		callback bool
		want     bool
	}{
		{"usable", `,"usage":{"input_tokens":123,"output_tokens":1}`, false, true},
		{"missing", "", false, false},
		{"missing output", `,"usage":{"input_tokens":123}`, false, false},
		{"negative", `,"usage":{"input_tokens":-1,"output_tokens":1}`, false, false},
		{"callback error", `,"usage":{"input_tokens":123,"output_tokens":1}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"measured\""+tc.usage+"}}\n\n")
			}))
			defer server.Close()
			a := NewAdapter(Config{BaseURL: server.URL, TokenSource: func(context.Context) (AccessTokens, error) {
				return AccessTokens{Access: "fake", AccountID: "fake"}, nil
			}})
			actual := tokenRequest("actual")
			other := tokenRequest("estimated concurrently")
			err := a.Stream(context.Background(), actual, func(event llm.StreamEvent) error {
				// Estimation during an unrelated request must not rebind feedback.
				estimated(t, a, other)
				if tc.callback {
					return errors.New("consumer failed")
				}
				return nil
			})
			if (err != nil) != tc.callback {
				t.Fatal("unexpected stream result", err)
			}
			got := estimated(t, a, actual)
			if got.Estimated == tc.want || tc.want && got.Input != 123 {
				t.Fatalf("incorrect measurement learning: %+v", got)
			}
			if got := estimated(t, a, other); !got.Estimated {
				t.Fatal("usage learned on unrelated request", got)
			}
		})
	}
}

func TestConcurrentStreamsLearnTheirOwnInput(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []struct {
				Content []struct{ Text string }
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		input := 101
		if request.Input[0].Content[0].Text == "two" {
			input = 202
		}
		arrived <- struct{}{}
		<-release
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":%d,\"output_tokens\":1}}}\n\n", input)
	}))
	defer server.Close()
	a := NewAdapter(Config{BaseURL: server.URL, TokenSource: func(context.Context) (AccessTokens, error) {
		return AccessTokens{Access: "fake", AccountID: "fake"}, nil
	}})
	results := make(chan error, 2)
	for _, text := range []string{"one", "two"} {
		go func() {
			results <- a.Stream(context.Background(), tokenRequest(text), func(llm.StreamEvent) error { return nil })
		}()
	}
	<-arrived
	<-arrived
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for text, want := range map[string]int{"one": 101, "two": 202} {
		if got := estimated(t, a, tokenRequest(text)); got.Estimated || got.Input != want {
			t.Fatal("concurrent stream learned another request", text, got)
		}
	}
}

type foreignPreparedInput struct{}

func (foreignPreparedInput) Provider() string { return "openai" }

func TestPreparedInputRejectsForeignAndModifiedRequests(t *testing.T) {
	a := &Adapter{}
	req := tokenRequest("one")
	req.Tools = []llm.ToolDefinition{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}
	first := estimated(t, a, req)
	req.Prepared = first.Prepared
	if got := estimated(t, a, req); got.Prepared != first.Prepared || *got.Checkpoint != *first.Checkpoint {
		t.Fatal("unchanged request did not reuse immutable preparation", got)
	}
	audited := req
	audited.Messages = append([]llm.Message(nil), req.Messages...)
	audited.Messages[0].EventSeq = 123
	audited.Messages[0].RequestID = 456
	audited.Messages[0].UserText = new(string)
	audited.Messages[0].InputTimeMS = 789
	audited.Messages[0].RetainedAtMS = 1000
	audited.Selection.Model.Name = "display name"
	audited.Selection.Model.Budget.ContextLimit = 10000
	if got := estimated(t, a, audited); got.Prepared != first.Prepared {
		t.Fatal("audit/display metadata invalidated provider preparation", got)
	}
	for _, edit := range []func(*llm.Request){
		func(r *llm.Request) { r.Prepared = foreignPreparedInput{} },
		func(r *llm.Request) { r.System += "changed" },
		func(r *llm.Request) { r.ConversationID += "changed" },
		func(r *llm.Request) { r.Selection.Variant = "high" },
		func(r *llm.Request) { r.NoTools = true },
		func(r *llm.Request) { r.Messages = []llm.Message{{Type: llm.TypeUserInput, Content: "changed"}} },
		func(r *llm.Request) { r.Tools = nil },
	} {
		changed := req
		edit(&changed)
		got, err := a.EstimateInput(context.Background(), changed)
		if err == nil || got.Input != -1 || got.Checkpoint != nil || got.Prepared != nil {
			t.Fatal("foreign or changed request reused frozen input", got, err)
		}
		if err := a.Stream(context.Background(), changed, func(llm.StreamEvent) error { return nil }); err == nil {
			t.Fatal("stream accepted foreign or changed preparation")
		}
	}
	// RawMessage slices remain caller-owned and mutable, so binding must check
	// their bytes rather than caching by slice identity.
	req.Tools[0].Parameters[2] = 'T'
	if _, err := a.EstimateInput(context.Background(), req); err == nil {
		t.Fatal("mutated raw schema reused frozen input")
	}
}

func TestPreparedInputFreezesAttachmentAvailability(t *testing.T) {
	for _, availableFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(availableFirst), func(t *testing.T) {
			var resolutions atomic.Int32
			sent := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				sent <- string(body)
				io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":123,\"output_tokens\":1}}}\n\n")
			}))
			defer server.Close()
			a := NewAdapter(Config{BaseURL: server.URL, TokenSource: func(context.Context) (AccessTokens, error) {
				return AccessTokens{Access: "fake", AccountID: "fake"}, nil
			}, ResolveBinary: func(_ context.Context, file llm.BinaryFile) (llm.BinaryPayload, error) {
				available := resolutions.Add(1) == 1 == availableFirst
				if !available {
					return llm.BinaryPayload{}, &llm.UnavailableBinaryFileError{File: file, Err: errors.New("gone")}
				}
				return llm.BinaryPayload{MIMEType: "image/png", Data: []byte("image")}, nil
			}})
			req := tokenRequest("one")
			req.Selection.Model.Images = true
			req.Messages[0].Files = []llm.BinaryFile{{Path: "/image.png", MIMEType: "image/png", Bytes: 5}}
			admission := estimated(t, a, req)
			req.Prepared = admission.Prepared
			if err := a.Stream(context.Background(), req, func(llm.StreamEvent) error { return nil }); err != nil {
				t.Fatal(err)
			}
			body := <-sent
			if resolutions.Load() != 1 || strings.Contains(body, `"type":"input_image"`) != availableFirst {
				t.Fatal("stream re-resolved or changed prepared attachment", resolutions.Load(), body)
			}
			if got := estimated(t, a, req); got.Estimated || got.Input != 123 || *got.Checkpoint != *admission.Checkpoint {
				t.Fatal("usage did not bind to admitted frozen input", got)
			}
			// A fresh preparation sees the availability change and must not use
			// usage measured for the other actual wire input.
			req.Prepared = nil
			if got := estimated(t, a, req); !got.Estimated || got.Checkpoint.Exact == admission.Checkpoint.Exact {
				t.Fatal("different wire preparation borrowed the old measurement", got)
			}
		})
	}
}
