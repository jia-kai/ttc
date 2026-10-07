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

func TestNoToolsRepliesRetryAtomicallyAfterPrivatePartialText(t *testing.T) {
	for _, terminal := range []string{"unknown", "protocol", "tool", "disconnect"} {
		t.Run(terminal, func(t *testing.T) {
			requests, retries := 0, 0
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Retry-After", "0")
				if requests == 1 {
					io.WriteString(w, sseFrames(partialOutput("text")))
					switch terminal {
					case "unknown":
						io.WriteString(w, sseFrames(map[string]any{"type": "error"}))
					case "protocol":
						io.WriteString(w, "data: invalid\n\n")
					case "tool":
						io.WriteString(w, sseFrames(partialOutput("call_start")))
					}
					return
				}
				io.WriteString(w, sseFrames(map[string]any{"type": "response.output_text.delta", "delta": "Final"}, messageDone(0, "Final"), map[string]any{"type": "response.completed"}))
			})
			var text strings.Builder
			var completed bool
			req := partialRequest(0, 0)
			req.NoTools = true
			err := a.Stream(context.Background(), req, func(ev llm.StreamEvent) error {
				switch ev.Kind {
				case "text":
					text.WriteString(ev.Text)
				case "retry":
					retries++
					if ev.Retry.MaxAttempts != llm.DefaultMaxAttempts || ev.Retry.DelayMilliseconds != 0 {
						t.Error("private reply did not use shared budget/backoff", ev.Retry)
					}
				case "completed":
					completed = true
				}
				return nil
			})
			if err != nil || requests != 2 || retries != 1 || text.String() != "Final" || !completed {
				t.Fatal("failed private text leaked or prevented recovery", err, requests, retries, text.String(), completed)
			}
		})
	}
}

func TestNoToolsCompletionCallbackFailureDoesNotRetry(t *testing.T) {
	for _, kind := range []string{"text", "state", "completed"} {
		t.Run(kind, func(t *testing.T) {
			requests := 0
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				io.WriteString(w, sseFrames(map[string]any{"type": "response.output_text.delta", "delta": "Final"}, messageDone(0, "Final"), map[string]any{"type": "response.completed"}))
			})
			failure := &llm.PartialError{Err: errors.New("local write failed"), Retry: llm.Retry{Attempt: 2, MaxAttempts: 3}}
			req := partialRequest(0, 0)
			req.NoTools = true
			err := a.Stream(context.Background(), req, func(ev llm.StreamEvent) error {
				if ev.Kind == kind {
					return failure
				}
				return nil
			})
			var partial *llm.PartialError
			if !errors.Is(err, failure) || errors.As(err, &partial) || requests != 1 {
				t.Fatal("private reply callback authorized recovery", err, requests)
			}
		})
	}
}

func TestInvalidLocalHeadersNeverReachTransportOrRetry(t *testing.T) {
	for _, field := range []string{"access", "account", "routing"} {
		t.Run(field, func(t *testing.T) {
			requests, retries := 0, 0
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) { requests++ })
			tokens := AccessTokens{Access: "token", AccountID: "account"}
			if field == "access" {
				tokens.Access = "private\nvalue"
			} else if field == "account" {
				tokens.AccountID = "private\nvalue"
			}
			a.tokenSource = func(context.Context) (AccessTokens, error) { return tokens, nil }
			req := partialRequest(0, 0)
			if field == "routing" {
				req.Selection.Model.ServiceTier = "private\nvalue"
			}
			err := a.Stream(context.Background(), req, func(ev llm.StreamEvent) error { retries++; return nil })
			var upstream *llm.TransientError
			if err == nil || requests != 0 || retries != 0 || errors.As(err, &upstream) || strings.Contains(err.Error(), "private") {
				t.Fatal("local headers leaked or authorized retry", err, requests, retries)
			}
		})
	}
}

func TestReplayValidationFailureDoesNotLogPrivateScalar(t *testing.T) {
	requests := 0
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		// Overflowing map[string]any decoding must not put the complete private
		// scalar from an unrelated reasoning field into the failure diagnostic.
		item := `{"type":"reasoning","id":"private","encrypted_content":"secret","unknown":` + strings.Repeat("9", 5000) + `}`
		io.WriteString(w, `data: {"type":"response.output_item.done","output_index":0,"item":`+item+"}\n\n")
		io.WriteString(w, sseFrames(map[string]any{"type": "response.completed"}))
	})
	err := a.Stream(context.Background(), partialRequest(0, 1), func(ev llm.StreamEvent) error {
		t.Error("invalid private state escaped", ev.Kind)
		return nil
	})
	if err == nil || requests != 1 || len(err.Error()) > 512 || strings.Contains(err.Error(), "9999") || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "invalid OpenAI replay item JSON") {
		t.Fatal("private scalar leaked into error", err, requests)
	}
}
