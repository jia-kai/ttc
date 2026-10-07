package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestUncommittedUpstreamFailureMatrixRecoversOrExhausts(t *testing.T) {
	completed := map[string]any{"type": "response.completed"}
	done := messageDone(0, "")
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"bare error", sseFrames(map[string]any{"type": "error"}), []string{"stream terminated: error", "error details missing"}},
		{"unknown nested code", sseFrames(map[string]any{"type": "error", "error": map[string]string{"type": "unexpected_error", "code": "unknown_code", "message": "Try fewer tools", "param": "tools"}}), []string{"type=unexpected_error", "code=unknown_code", "message=Try fewer tools", "parameter=tools"}},
		{"unknown nested shape", sseFrames(map[string]any{"type": "error", "error": map[string]any{"new_field": "private scalar"}}), []string{"error details missing", `"new_field":string`}},
		{"failed response", sseFrames(map[string]any{"type": "response.failed", "response": map[string]any{"id": "response_fixture", "error": map[string]string{"code": "invalid_request_error", "message": "Unsupported request option"}}}), []string{"response.failed", "response_id=response_fixture", "invalid_request_error", "Unsupported request option"}},
		{"bare failed response", sseFrames(map[string]any{"type": "response.failed"}), []string{"response.failed", "error details missing"}},
		{"context length", sseFrames(map[string]any{"type": "error", "code": "context_length_exceeded", "message": "Compact the conversation"}), []string{"context_length_exceeded", "Compact the conversation"}},
		{"incomplete response", sseFrames(map[string]any{"type": "response.incomplete", "response": map[string]any{"incomplete_details": map[string]string{"reason": "max_output_tokens"}}}), []string{"response.incomplete", "reason=max_output_tokens"}},
		{"unexpected terminal field type", `data: {"type":"error","response":{"id":123},"error":{"message":"Try another request"}}` + "\n\n", []string{"stream terminated: error", "Try another request"}},
		{"malformed JSON", "data: invalid JSON containing private scalar\n\n", []string{"invalid Responses stream JSON"}},
		{"duplicate output index", sseFrames(done, done, completed), []string{"duplicate completed Responses output index"}},
		{"inconsistent output", sseFrames(messageDone(0, "unannounced text"), completed), []string{"replay state disagrees with canonical response"}},
		{"empty stream", "", []string{"stream interrupted before completion"}},
		{"oversized SSE", "data: " + strings.Repeat("x", 8<<20) + "\n\n", []string{"stream line exceeds 8 MiB"}},
	} {
		for _, recover := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recover=%t", tc.name, recover), func(t *testing.T) {
				requests := 0
				a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
					requests++
					w.Header().Set("Retry-After", "0")
					w.Header().Set("x-request-id", "request_fixture")
					if recover && requests == 2 {
						io.WriteString(w, sseFrames(completed))
						return
					}
					io.WriteString(w, tc.body)
				})
				// Use a distinct credential: the shared fixture's short "token"
				// also occurs in the legitimate max_output_tokens diagnostic.
				a.tokenSource = func(context.Context) (AccessTokens, error) {
					return AccessTokens{Access: "fixture-access-credential", AccountID: "account"}, nil
				}
				var retries []llm.Retry
				completions := 0
				err := a.Stream(context.Background(), partialRequest(0, 0), func(ev llm.StreamEvent) error {
					switch ev.Kind {
					case "retry":
						retries = append(retries, *ev.Retry)
					case "completed":
						completions++
					default:
						t.Errorf("failed attempt leaked output: %s", ev.Kind)
					}
					return nil
				})
				wantRequests, wantRetries, wantCompletions := llm.DefaultMaxAttempts, llm.DefaultMaxAttempts-1, 0
				if recover {
					wantRequests, wantRetries, wantCompletions = 2, 1, 1
					if err != nil {
						t.Fatal("failed to recover", err)
					}
				} else {
					var transient *llm.TransientError
					var partial *llm.PartialError
					if !errors.As(err, &transient) || errors.As(err, &partial) || !strings.HasPrefix(err.Error(), "upstream attempt 3/3 failed: ") {
						t.Fatal("incorrect exhaustion", err)
					}
				}
				if requests != wantRequests || len(retries) != wantRetries || completions != wantCompletions {
					t.Fatal("incorrect attempt budget", requests, retries, completions)
				}
				for i, retry := range retries {
					prefix := fmt.Sprintf("upstream attempt %d/3 failed: ", i+1)
					if retry.Attempt != i+2 || retry.MaxAttempts != 3 || retry.DelayMilliseconds != 0 || !strings.HasPrefix(retry.Reason, prefix) {
						t.Fatal("incorrect retry metadata", retry)
					}
					for _, want := range append(tc.want, "request_id=request_fixture") {
						if !strings.Contains(retry.Reason, want) {
							t.Errorf("retry lost %q: %s", want, retry.Reason)
						}
					}
					if strings.Contains(retry.Reason, "private scalar") {
						t.Error("retry exposed arbitrary upstream value", retry.Reason)
					}
					if !recover && strings.TrimPrefix(retry.Reason, prefix) != strings.TrimPrefix(err.Error(), "upstream attempt 3/3 failed: ") {
						t.Fatal("retry reason did not preserve full final diagnostic", retry.Reason, err)
					}
				}
			})
		}
	}
}

func TestNoToolsUpstreamFailuresExhaustWithoutPublishingIncompleteOutput(t *testing.T) {
	for _, terminal := range []string{"disconnect", "malformed", "unexpected tool"} {
		t.Run(terminal, func(t *testing.T) {
			requests := 0
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Retry-After", "0")
				io.WriteString(w, sseFrames(partialOutput("text")))
				switch terminal {
				case "malformed":
					io.WriteString(w, "data: invalid JSON\n\n")
				case "unexpected tool":
					io.WriteString(w, sseFrames(partialOutput("call_start")))
				}
			})
			req := partialRequest(0, 2)
			req.NoTools = true
			retries := 0
			err := a.Stream(context.Background(), req, func(ev llm.StreamEvent) error {
				if ev.Kind != "retry" {
					t.Error("failed no-tools generation published output", ev.Kind)
				} else {
					retries++
				}
				return nil
			})
			var partial *llm.PartialError
			var transient *llm.TransientError
			if requests != 2 || retries != 1 || errors.As(err, &partial) || !errors.As(err, &transient) || !strings.HasPrefix(err.Error(), "upstream attempt 2/2 failed: ") {
				t.Fatal("buffered failure handed off or bypassed budget", err, requests, retries)
			}
		})
	}
}

func TestLocalRequestFailuresNeverRetry(t *testing.T) {
	for _, kind := range []string{"selection", "conversation", "history", "credentials missing", "credentials invalid", "credentials header", "credentials failure", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			requests := 0
			a := adapterFixture(t, func(http.ResponseWriter, *http.Request) { requests++ })
			req := partialRequest(0, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("credentials unavailable")
			switch kind {
			case "selection":
				req.Selection.Provider = "other"
			case "conversation":
				req.ConversationID = ""
			case "history":
				req.Messages = []llm.Message{{Role: "assistant", Calls: []llm.ToolCall{{ID: "call", Name: "read", Arguments: []byte(`invalid`)}}}}
			case "credentials missing":
				a.tokenSource = nil
			case "credentials invalid":
				a.tokenSource = func(context.Context) (AccessTokens, error) { return AccessTokens{}, nil }
			case "credentials header":
				a.tokenSource = func(context.Context) (AccessTokens, error) {
					return AccessTokens{Access: "token\ninvalid", AccountID: "account"}, nil
				}
			case "credentials failure":
				a.tokenSource = func(context.Context) (AccessTokens, error) { return AccessTokens{}, failure }
			case "cancelled":
				cancel()
			}
			err := a.Stream(ctx, req, func(ev llm.StreamEvent) error {
				t.Error("local failure emitted event", ev.Kind)
				return nil
			})
			var transient *llm.TransientError
			var partial *llm.PartialError
			if err == nil || errors.As(err, &transient) || errors.As(err, &partial) || requests != 0 {
				t.Fatal("local failure retried or granted recovery", err, requests)
			}
			if kind == "cancelled" && !errors.Is(err, context.Canceled) || kind == "credentials failure" && !errors.Is(err, failure) {
				t.Fatal("local failure lost identity", err)
			}
		})
	}
}
