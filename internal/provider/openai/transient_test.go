package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"scicode/internal/provider"
)

func TestTerminalStreamFailuresCarryRecoverability(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		transient bool
	}{
		{"rate limit", 429, "", true}, {"server failure", 503, "", true},
		{"invalid input", 400, "", false},
		{"lost after output", 200, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", true},
		{"malformed JSON", 200, "data: invalid\n\n", false},
		{"server event", 200, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"}}}\n\n", true},
		{"context event", 200, "data: {\"type\":\"error\",\"code\":\"context_length_exceeded\"}\n\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.status); io.WriteString(w, test.body) })
			req := provider.Request{ConversationID: "test-conversation", Selection: provider.Selection{Provider: "openai", Model: provider.ScriptModel()}, MaxAttempts: 1}
			err := a.Stream(context.Background(), req, func(provider.StreamEvent) error { return nil })
			var transient *provider.TransientError
			if err == nil || errors.As(err, &transient) != test.transient {
				t.Fatal("wrong terminal classification", err)
			}
		})
	}
}

func TestTerminalTransportErrorDoesNotExposePrivateDetails(t *testing.T) {
	a := adapterFixture(t, func(http.ResponseWriter, *http.Request) {})
	a.Client = &http.Client{Transport: retryTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("private transport detail") })}
	err := a.Stream(context.Background(), provider.Request{ConversationID: "test-conversation", Selection: provider.Selection{Provider: "openai", Model: provider.ScriptModel()}, MaxAttempts: 1}, func(provider.StreamEvent) error { return nil })
	var transient *provider.TransientError
	if !errors.As(err, &transient) || strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe or unclassified transport error", err)
	}
}

func TestUncommittedTransientSSEFailuresRetry(t *testing.T) {
	for _, code := range []string{"server_error", "rate_limit_exceeded", "temporarily_unavailable"} {
		for _, kind := range []string{"response.failed", "error"} {
			t.Run(kind+"/"+code, func(t *testing.T) {
				requests, retries := 0, 0
				a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
					requests++
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
				err := a.Stream(context.Background(), provider.Request{ConversationID: "test-conversation", Selection: provider.Selection{Provider: "openai", Model: provider.ScriptModel()}, MaxAttempts: 2}, func(ev provider.StreamEvent) error {
					if ev.Kind == "retry" {
						retries++
						if ev.Retry.Attempt != 2 || ev.Retry.MaxAttempts != 2 {
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
			callbackErr := &provider.TransientError{Err: errors.New("callback persistence failure")}
			err := a.Stream(context.Background(), provider.Request{ConversationID: "test-conversation", Selection: provider.Selection{Provider: "openai", Model: provider.ScriptModel()}, MaxAttempts: 2}, func(ev provider.StreamEvent) error {
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
