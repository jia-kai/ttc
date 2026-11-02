package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestContextLengthRejectionStopsHTTPAndSSEWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"HTTP nested", 400, `{"error":{"code":"context_length_exceeded","message":"Input too large"}}`},
		{"HTTP root", 413, `{"code":"context_length_exceeded","message":"Input too large"}`},
		{"SSE root", 200, "data: {\"type\":\"error\",\"code\":\"context_length_exceeded\"}\n\n"},
		{"SSE nested", 200, "data: {\"type\":\"error\",\"error\":{\"code\":\"context_length_exceeded\"}}\n\n"},
		{"SSE failed", 200, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"capacity-response\",\"error\":{\"code\":\"context_length_exceeded\"}}}\n\n"},
		{"SSE unexpected field type", 200, "data: {\"type\":\"response.failed\",\"response\":{\"id\":123,\"error\":{\"code\":\"context_length_exceeded\"}}}\n\n"},
	} {
		for _, noTools := range []bool{false, true} {
			for _, prior := range []int{0, 2} {
				t.Run(fmt.Sprintf("%s/no_tools=%t/prior=%d", tc.name, noTools, prior), func(t *testing.T) {
					var requests atomic.Int32
					a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						// Even a long Retry-After must not start a provider wait.
						w.Header().Set("Retry-After", "120")
						w.Header().Set("x-request-id", "capacity-request")
						w.WriteHeader(tc.status)
						io.WriteString(w, tc.body)
					})
					req := partialRequest(prior, 0)
					req.NoTools = noTools
					req.Messages = []llm.Message{{Type: llm.TypeUserInput, Content: "Retain this input"}}
					original := append([]llm.Message(nil), req.Messages...)
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					err := a.Stream(ctx, req, func(ev llm.StreamEvent) error {
						t.Errorf("rejected request emitted %s", ev.Kind)
						return nil
					})
					assertContextLengthRejection(t, err)
					if requests.Load() != 1 || !reflect.DeepEqual(req.Messages, original) {
						t.Fatal("rejection retried or changed source input", requests.Load(), req.Messages)
					}
					if !strings.Contains(err.Error(), "request_id=capacity-request") {
						t.Fatal("lost request diagnostic", err)
					}
				})
			}
		}
	}
}

func TestContextLengthRejectionAfterPartialOutputNeverAuthorizesContinuation(t *testing.T) {
	for _, output := range []string{"text", "call_start", "finished_call"} {
		for _, noTools := range []bool{false, true} {
			// NoTools rejects tool events before any later capacity rejection.
			if noTools && output != "text" {
				continue
			}
			t.Run(fmt.Sprintf("%s/no_tools=%t", output, noTools), func(t *testing.T) {
				var requests atomic.Int32
				a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if output == "finished_call" {
						for _, ev := range callEvents(0, "fixture_item", "fixture_call", "read", `{}`) {
							io.WriteString(w, sseFrames(ev))
						}
					} else {
						io.WriteString(w, sseFrames(partialOutput(output)))
					}
					io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded\"}}}\n\n")
				})
				req := partialRequest(0, 0)
				req.NoTools = noTools
				var kinds []string
				err := a.Stream(context.Background(), req, func(ev llm.StreamEvent) error {
					kinds = append(kinds, ev.Kind)
					return nil
				})
				assertContextLengthRejection(t, err)
				want := []string{output}
				if output == "finished_call" {
					want = []string{"call_start", "call_progress", "call_progress"}
				}
				if noTools {
					want = nil
				}
				if requests.Load() != 1 || !slices.Equal(kinds, want) {
					t.Fatal("replayed partial response, leaked private/executable output, or lost committed output", requests.Load(), kinds)
				}
			})
		}
	}
}

func assertContextLengthRejection(t *testing.T, err error) {
	t.Helper()
	var capacity *llm.ContextLengthError
	var partial *llm.PartialError
	var transient *llm.TransientError
	if !errors.As(err, &capacity) || errors.As(err, &partial) || errors.As(err, &transient) {
		t.Fatal("capacity rejection lost type or authorized retry", err)
	}
	for _, text := range []string{"context_length_exceeded", "unchanged retries stopped", "Explicitly compact", "larger context window"} {
		if !strings.Contains(err.Error(), text) {
			t.Errorf("missing recovery diagnostic %q: %v", text, err)
		}
	}
}

func TestContextLengthTextAndUnknownCodesRetainRetryPolicy(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"context_length_exceeded"}}`,
		`{"error":{"type":"context_length_exceeded"}}`,
		`{"error":{"code":"prefix_context_length_exceeded"}}`,
		`{"error":{"code":["context_length_exceeded"]}}`,
		`{"error":{"code":"CONTEXT_LENGTH_EXCEEDED"}}`,
		`{"extra":{"code":"context_length_exceeded"}}`,
		`{"response":{"incomplete_details":{"reason":"context_length_exceeded"}}}`,
		`{"error":{"code":"context_length_exceeded"}`, // Malformed JSON is not a structured rejection.
	} {
		for _, status := range []int{400, 429, 500, 200} {
			t.Run(fmt.Sprintf("%d/%s", status, body), func(t *testing.T) {
				var requests atomic.Int32
				a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", "0")
					if requests.Add(1) == 2 {
						io.WriteString(w, sseFrames(map[string]any{"type": "response.completed"}))
						return
					}
					w.WriteHeader(status)
					if status == 200 {
						fmt.Fprintf(w, "data: {\"type\":\"error\",%s\n\n", body[1:])
					} else {
						io.WriteString(w, body)
					}
				})
				retries, completions := 0, 0
				err := a.Stream(context.Background(), partialRequest(0, 2), func(ev llm.StreamEvent) error {
					switch ev.Kind {
					case "retry":
						retries++
					case "completed":
						completions++
					default:
						t.Errorf("unexpected event %s", ev.Kind)
					}
					return nil
				})
				if err != nil || requests.Load() != 2 || retries != 1 || completions != 1 {
					t.Fatal("non-capacity failure changed retry behavior", err, requests.Load(), retries, completions)
				}
			})
		}
	}
}
