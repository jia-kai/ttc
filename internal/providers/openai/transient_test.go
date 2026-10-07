package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestTerminalStreamFailuresCarryRecoverability(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"rate limit", 429, ""}, {"server failure", 503, ""},
		{"invalid input", 400, ""},
		{"lost after output", 200, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"},
		{"malformed JSON", 200, "data: invalid\n\n"},
		{"server event", 200, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"}}}\n\n"},
		{"context event", 200, "data: {\"type\":\"error\",\"code\":\"context_length_exceeded\"}\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.status); io.WriteString(w, test.body) })
			req := llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, MaxAttempts: 1}
			err := a.Stream(context.Background(), req, func(llm.StreamEvent) error { return nil })
			var transient *llm.TransientError
			var partial *llm.PartialError
			if !errors.As(err, &transient) || errors.As(err, &partial) || !strings.HasPrefix(err.Error(), "upstream attempt 1/1 failed: ") {
				t.Fatal("wrong terminal classification", err)
			}
		})
	}
}

func TestTerminalTransportErrorDoesNotExposePrivateDetails(t *testing.T) {
	a := adapterFixture(t, func(http.ResponseWriter, *http.Request) {})
	a.Client = &http.Client{Transport: retryTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("private transport detail") })}
	err := a.Stream(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, MaxAttempts: 1}, func(llm.StreamEvent) error { return nil })
	var transient *llm.TransientError
	if !errors.As(err, &transient) || strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe or unclassified transport error", err)
	}
}

func TestUncommittedSSEFailuresRetryRegardlessOfCode(t *testing.T) {
	for _, code := range []string{"server_error", "rate_limit_exceeded", "temporarily_unavailable", "context_length_exceeded", "invalid_request_error", "unknown_failure"} {
		for _, kind := range []string{"response.failed", "error"} {
			t.Run(kind+"/"+code, func(t *testing.T) {
				requests, retries := 0, 0
				a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
					requests++
					w.Header().Set("Retry-After", "0")
					if requests == 1 {
						event := map[string]any{"type": kind, "code": code, "message": "Temporary fixture failure"}
						if kind == "response.failed" {
							event["response"] = map[string]any{"error": map[string]string{"code": code, "message": "Temporary fixture failure"}}
						}
						io.WriteString(w, sseFrames(event))
						return
					}
					io.WriteString(w, sseFrames(map[string]any{"type": "response.completed"}))
				})
				err := a.Stream(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, MaxAttempts: 2}, func(ev llm.StreamEvent) error {
					if ev.Kind == "retry" {
						retries++
						if ev.Retry.Attempt != 2 || ev.Retry.MaxAttempts != 2 || ev.Retry.DelayMilliseconds != 0 || !strings.Contains(ev.Retry.Reason, "upstream attempt 1/2 failed: ") || !strings.Contains(ev.Retry.Reason, code) || !strings.Contains(ev.Retry.Reason, "Temporary fixture failure") {
							t.Error("incorrect retry notice", ev.Retry)
						}
					}
					return nil
				})
				if err != nil || requests != 2 || retries != 1 {
					t.Fatal("transient SSE did not recover", err, requests, retries)
				}
			})
		}
	}
}

func TestStreamDoesNotRetryCommittedOutputOrCallbackFailure(t *testing.T) {
	for _, mode := range []string{"text", "tool", "callback"} {
		t.Run(mode, func(t *testing.T) {
			requests, retries := 0, 0
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				first := map[string]any{"type": "response.output_text.delta", "delta": "partial"}
				if mode == "tool" {
					first = callEvents(0, "fixture_item", "fixture_call", "read", `{}`)[0]
				}
				io.WriteString(w, sseFrames(first, map[string]any{"type": "error", "code": "server_error", "message": "Temporary fixture failure"}))
			})
			callbackErr := &llm.TransientError{Err: errors.New("callback persistence failure")}
			err := a.Stream(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, MaxAttempts: 2}, func(ev llm.StreamEvent) error {
				if ev.Kind == "retry" {
					retries++
				}
				if mode == "callback" {
					return callbackErr
				}
				return nil
			})
			if err == nil || requests != 1 || retries != 0 || mode == "callback" && !errors.Is(err, callbackErr) {
				t.Fatal("unsafe retry or changed callback error", err, requests, retries)
			}
		})
	}
}

func TestBufferedReasoningTransientFailuresRetryWithoutLeakingState(t *testing.T) {
	for _, terminal := range []string{"disconnect", "server error"} {
		t.Run(terminal, func(t *testing.T) {
			requests, retries, states, completions := 0, 0, 0, 0
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Retry-After", "0")
				if requests > 1 {
					io.WriteString(w, sseFrames(map[string]any{
						"type": "response.output_item.done", "output_index": 0,
						"item": map[string]any{"type": "reasoning", "id": "successful_reasoning", "encrypted_content": "retained", "summary": []any{}},
					}, map[string]any{"type": "response.completed"}))
					return
				}
				io.WriteString(w, sseFrames(map[string]any{
					"type": "response.output_item.done", "output_index": 0,
					"item": map[string]any{"type": "reasoning", "id": "private_reasoning", "encrypted_content": "discarded", "summary": []any{}},
				}))
				if terminal == "server error" {
					io.WriteString(w, sseFrames(map[string]any{"type": "error", "code": "server_error"}))
				}
			})
			err := a.Stream(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, MaxAttempts: 2}, func(ev llm.StreamEvent) error {
				switch ev.Kind {
				case "retry":
					retries++
				case "completed":
					completions++
				case "state":
					states++
					if strings.Contains(string(ev.StateItem), "discarded") || !strings.Contains(string(ev.StateItem), "retained") {
						t.Errorf("failed attempt leaked state: %s", ev.StateItem)
					}
				default:
					t.Errorf("failed attempt leaked output: %s", ev.Kind)
				}
				return nil
			})
			if err != nil || requests != 2 || retries != 1 || states != 1 || completions != 1 {
				t.Fatal("buffered reasoning prevented safe retry", err, requests, retries, states, completions)
			}
		})
	}
}

func TestCompletionCallbackFailuresNeverRetryBufferedOutput(t *testing.T) {
	for _, kind := range []string{"state", "phase"} {
		for _, failure := range []error{&llm.TransientError{Err: errors.New("callback failed")}, errStreamLost} {
			t.Run(kind+"/"+failure.Error(), func(t *testing.T) {
				requests, retries := 0, 0
				item := map[string]any{"type": "reasoning", "id": "buffered", "encrypted_content": "private", "summary": []any{}}
				if kind == "phase" {
					item = map[string]any{"type": "message", "id": "buffered", "role": "assistant", "phase": "final_answer", "content": []any{}}
				}
				a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
					requests++
					io.WriteString(w, sseFrames(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}, map[string]any{"type": "response.completed"}))
				})
				err := a.Stream(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, MaxAttempts: 2}, func(ev llm.StreamEvent) error {
					if ev.Kind == "retry" {
						retries++
					}
					if ev.Kind == kind {
						return failure
					}
					return nil
				})
				if err != failure || requests != 1 || retries != 0 {
					t.Fatal("callback failure retried or lost", err, requests, retries)
				}
			})
		}
	}
}
