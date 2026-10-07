package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/llm"
)

func partialRequest(prior, limit int) llm.Request {
	return llm.Request{
		ConversationID: "test-conversation",
		Selection:      llm.Selection{Provider: "openai", Model: llm.ScriptModel()},
		PriorAttempts:  prior, MaxAttempts: limit,
	}
}

func partialOutput(kind string) map[string]any {
	if kind == "call_start" {
		return callEvents(0, "fixture_item", "fixture_call", "read", `{}`)[0]
	}
	return map[string]any{"type": "response.output_text.delta", "delta": "partial"}
}

func TestCommittedUpstreamFailuresHandOffWithoutReplay(t *testing.T) {
	for _, output := range []string{"text", "call_start", "finished_call"} {
		for _, terminal := range []string{"disconnect", "error", "response.failed", "bare error", "unknown nested", "context length", "malformed JSON", "duplicate output"} {
			t.Run(output+"/"+terminal, func(t *testing.T) {
				var requests atomic.Int32
				a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if output == "finished_call" {
						for _, event := range callEvents(0, "fixture_item", "fixture_call", "read", `{}`) {
							io.WriteString(w, sseFrames(event))
						}
					} else {
						io.WriteString(w, sseFrames(partialOutput(output)))
					}
					switch terminal {
					case "error":
						io.WriteString(w, sseFrames(map[string]any{"type": "error", "code": "server_error", "message": "private backend detail"}))
					case "response.failed":
						io.WriteString(w, sseFrames(map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]string{"code": "temporarily_unavailable", "message": "private backend detail"}}}))
					case "bare error":
						io.WriteString(w, sseFrames(map[string]any{"type": "error"}))
					case "unknown nested":
						io.WriteString(w, sseFrames(map[string]any{"type": "error", "error": map[string]string{"code": "unknown_failure", "message": "Reduce tool count"}}))
					case "context length":
						io.WriteString(w, sseFrames(map[string]any{"type": "error", "code": "context_length_exceeded"}))
					case "malformed JSON":
						io.WriteString(w, "data: invalid JSON\n\n")
					case "duplicate output":
						done := messageDone(9, "partial")
						io.WriteString(w, sseFrames(done, done))
					}
				})
				// A provider-owned wait would exhaust this deadline instead of handing off.
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()
				var kinds []string
				err := a.Stream(ctx, partialRequest(0, 2), func(ev llm.StreamEvent) error {
					kinds = append(kinds, ev.Kind)
					return nil
				})
				var partial *llm.PartialError
				var transient *llm.TransientError
				wantKind := output
				wantKinds := []string{wantKind}
				if output == "finished_call" {
					wantKinds = []string{"call_start", "call_progress", "call_progress"}
				}
				if !errors.As(err, &partial) || !errors.As(err, &transient) || requests.Load() != 1 || !slices.Equal(kinds, wantKinds) {
					t.Fatal("missing handoff, replay, wait or leaked executable output", err, requests.Load(), kinds)
				}
				retry := partial.Retry
				if retry.Attempt != 2 || retry.MaxAttempts != 2 || retry.DelayMilliseconds < 750 || retry.DelayMilliseconds > 1250 || retry.Reason != transient.Error() || !strings.HasPrefix(retry.Reason, "upstream attempt 1/2 failed: ") {
					t.Fatal("invalid or unsafe continuation metadata", retry)
				}
			})
		}
	}
}

func TestInvalidFinalToolArgumentsUsePartialRecovery(t *testing.T) {
	for _, kind := range []string{"invalid_json", "conflicting_final"} {
		t.Run(kind, func(t *testing.T) {
			var requests atomic.Int32
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				events := callEvents(0, "item", "call", "read", `{"path":"x"}`)
				if kind == "invalid_json" {
					events[3]["arguments"] = `{"path":`
				} else {
					events[4]["item"].(map[string]any)["arguments"] = `{"path":"other"}`
				}
				io.WriteString(w, sseFrames(events[0], events[1], events[2], events[3], events[4], map[string]any{"type": "response.completed"}))
			})
			for _, tc := range []struct {
				prior, limit int
				handoff      bool
			}{{0, 2, true}, {1, 2, false}, {2, 0, false}} {
				var starts, calls int
				err := a.Stream(context.Background(), partialRequest(tc.prior, tc.limit), func(ev llm.StreamEvent) error {
					if ev.Kind == "call_start" {
						starts++
					}
					if ev.Kind == "call" {
						calls++
					}
					return nil
				})
				var partial *llm.PartialError
				var transient *llm.TransientError
				if starts != 1 || calls != 0 || !errors.As(err, &transient) || errors.As(err, &partial) != tc.handoff {
					t.Fatal("invalid tool call executed or retry policy was ignored", tc, err, starts, calls)
				}
				if partial != nil && (partial.Retry.Attempt != tc.prior+2 || partial.Retry.Reason == "") {
					t.Fatal("invalid recovery metadata", partial.Retry)
				}
			}
			if requests.Load() != 3 {
				t.Fatal("unexpected request count", requests.Load())
			}
		})
	}
}

func TestPartialHandoffRespectsPriorAttemptsAndLimits(t *testing.T) {
	for _, tc := range []struct {
		prior, limit int
		handoff      bool
		minMS, maxMS int64
	}{
		{0, 0, true, 750, 1250},
		{0, 1, false, 0, 0},
		{1, 2, false, 0, 0},
		{1, 3, true, 1500, 2500},
		{3, 5, true, 6000, 10000},
		{1, 0, true, 1500, 2500},
		{2, 0, false, 0, 0},
		{100000, 100002, true, 22500, 30000},
	} {
		t.Run(fmt.Sprintf("%d/%d", tc.prior, tc.limit), func(t *testing.T) {
			var requests atomic.Int32
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				io.WriteString(w, sseFrames(partialOutput("text")))
			})
			err := a.Stream(context.Background(), partialRequest(tc.prior, tc.limit), func(ev llm.StreamEvent) error {
				if ev.Kind != "text" {
					t.Error("unexpected replay or notice", ev.Kind)
				}
				return nil
			})
			var partial *llm.PartialError
			var transient *llm.TransientError
			if errors.As(err, &partial) != tc.handoff || !errors.As(err, &transient) || requests.Load() != 1 || !errors.Is(err, errStreamLost) {
				t.Fatal("incorrect bound or terminal error", err, requests.Load())
			}
			if tc.handoff {
				retry := partial.Retry
				limit := tc.limit
				if limit == 0 {
					limit = llm.DefaultMaxAttempts
				}
				if partial.Err != transient || retry.Attempt != tc.prior+2 || retry.MaxAttempts != limit || retry.Reason != transient.Error() || retry.DelayMilliseconds < tc.minMS || retry.DelayMilliseconds > tc.maxMS {
					t.Fatal("attempts or backoff restarted", partial, retry)
				}
			} else if err != transient {
				t.Fatal("exhausted budget wrapped original transient failure", err)
			}
		})
	}
}

func TestPriorAttemptsValidationMakesNoRequests(t *testing.T) {
	for _, bounds := range [][2]int{{-1, 0}, {0, -1}, {2, 2}, {3, 2}, {3, 0}, {100000, 0}} {
		var requests atomic.Int32
		a := adapterFixture(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) })
		err := a.Stream(context.Background(), partialRequest(bounds[0], bounds[1]), func(ev llm.StreamEvent) error {
			t.Error("invalid request emitted", ev.Kind)
			return nil
		})
		var partial *llm.PartialError
		var transient *llm.TransientError
		if err == nil || requests.Load() != 0 || errors.As(err, &partial) || errors.As(err, &transient) {
			t.Fatal("invalid attempts accepted or classified transient", bounds, err, requests.Load())
		}
	}
}

func TestPreOutputRetriesContinueAttemptNumberingIntoPartialHandoff(t *testing.T) {
	for _, limit := range []int{6, 7} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			var requests atomic.Int32
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) <= 2 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(503)
					return
				}
				io.WriteString(w, sseFrames(partialOutput("text")))
			})
			var retries []llm.Retry
			err := a.Stream(context.Background(), partialRequest(3, limit), func(ev llm.StreamEvent) error {
				if ev.Kind == "retry" {
					retries = append(retries, *ev.Retry)
				}
				return nil
			})
			var partial *llm.PartialError
			if err == nil || requests.Load() != 3 || len(retries) != 2 || errors.As(err, &partial) != (limit == 7) {
				t.Fatal("lost continuous attempt budget", err, requests.Load(), retries)
			}
			for i, retry := range retries {
				reason := fmt.Sprintf("upstream attempt %d/%d failed: subscription response HTTP 503: error details missing; body absent", i+4, limit)
				if retry != (llm.Retry{Attempt: i + 5, MaxAttempts: limit, Reason: reason}) {
					t.Fatal("pre-output numbering restarted", retry)
				}
			}
			if partial != nil && (partial.Retry.Attempt != 7 || partial.Retry.MaxAttempts != limit || partial.Retry.DelayMilliseconds < 22500 || partial.Retry.DelayMilliseconds > 30000) {
				t.Fatal("partial backoff restarted", partial.Retry)
			}
		})
	}
}

func TestMixedPreOutputAndPartialFailuresShareContinuationBudget(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(fmt.Sprint(recover), func(t *testing.T) {
			requests := 0
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Retry-After", "0")
				switch requests {
				case 1:
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":{"code":"invalid_request_error","message":"Adjust tools"}}`)
				case 2:
					io.WriteString(w, sseFrames(map[string]any{"type": "error", "error": map[string]string{"code": "unknown_backend_failure"}}))
				case 3:
					io.WriteString(w, sseFrames(partialOutput("text"), map[string]any{"type": "error", "code": "context_length_exceeded"}))
				case 4:
					if recover {
						io.WriteString(w, sseFrames(map[string]any{"type": "response.completed"}))
					} else {
						io.WriteString(w, sseFrames(partialOutput("text"), map[string]any{"type": "error"}))
					}
				default:
					t.Error("continued after exhausting shared budget", requests)
				}
			})
			var retries []llm.Retry
			emit := func(ev llm.StreamEvent) error {
				if ev.Kind == "retry" {
					retries = append(retries, *ev.Retry)
				}
				return nil
			}
			err := a.Stream(context.Background(), partialRequest(0, 4), emit)
			var partial *llm.PartialError
			if !errors.As(err, &partial) || requests != 3 || len(retries) != 2 {
				t.Fatal("pre-output attempts were lost or committed output replayed", err, requests, retries)
			}
			if partial.Retry.Attempt != 4 || partial.Retry.MaxAttempts != 4 || partial.Retry.DelayMilliseconds != 0 || partial.Retry.Reason != err.Error() || !strings.Contains(err.Error(), "upstream attempt 3/4 failed:") || !strings.Contains(err.Error(), "context_length_exceeded") {
				t.Fatal("incorrect continuation metadata", partial.Retry, err)
			}
			for i, retry := range retries {
				if retry.Attempt != i+2 || retry.MaxAttempts != 4 || !strings.HasPrefix(retry.Reason, fmt.Sprintf("upstream attempt %d/4 failed: ", i+1)) {
					t.Fatal("pre-output attempt numbering restarted", retry)
				}
			}
			err = a.Stream(context.Background(), partialRequest(partial.Retry.Attempt-1, partial.Retry.MaxAttempts), emit)
			if requests != 4 || len(retries) != 2 {
				t.Fatal("continuation reset shared budget", err, requests, retries)
			}
			if recover {
				if err != nil {
					t.Fatal("last allowed attempt failed to recover", err)
				}
			} else {
				var transient *llm.TransientError
				if !errors.As(err, &transient) || errors.As(err, &partial) || !strings.HasPrefix(err.Error(), "upstream attempt 4/4 failed: ") {
					t.Fatal("exhaustion granted another continuation", err)
				}
			}
		})
	}
}

func TestCallbackPartialErrorsNeverAuthorizeHandoff(t *testing.T) {
	for _, kind := range []string{"text", "call_start", "call", "retry", "state", "completed"} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrapped=%t", kind, wrapped), func(t *testing.T) {
				var requests atomic.Int32
				a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if kind == "retry" {
						w.Header().Set("Retry-After", "0")
						w.WriteHeader(503)
						return
					}
					if kind == "text" || kind == "call_start" {
						io.WriteString(w, sseFrames(partialOutput(kind)))
						return
					}
					if kind == "call" {
						io.WriteString(w, functionFrames(0, "item", "call", "read", `{}`))
						io.WriteString(w, sseFrames(map[string]any{"type": "response.completed"}))
						return
					}
					io.WriteString(w, sseFrames(map[string]any{
						"type": "response.output_item.done", "output_index": 0,
						"item": map[string]any{"type": "reasoning", "id": "buffered", "encrypted_content": "private", "summary": []any{}},
					}, map[string]any{"type": "response.completed"}))
				})
				cause := errors.New("callback persistence failure")
				var failure error = &llm.PartialError{Err: cause, Retry: llm.Retry{Attempt: 2}}
				if wrapped {
					failure = fmt.Errorf("callback failed: %w", failure)
				}
				callbacks := 0
				err := a.Stream(context.Background(), partialRequest(0, 2), func(ev llm.StreamEvent) error {
					if ev.Kind == kind {
						callbacks++
						return failure
					}
					return nil
				})
				var partial *llm.PartialError
				if errors.As(err, &partial) || !errors.Is(err, cause) || !errors.Is(err, failure) || err.Error() != failure.Error() || requests.Load() != 1 || callbacks != 1 {
					t.Fatal("callback granted recovery or lost identity/diagnostic", err, requests.Load(), callbacks)
				}
			})
		}
	}
}

func TestCommittedCancellationNeverHandsOff(t *testing.T) {
	var requests atomic.Int32
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.WriteString(w, sseFrames(partialOutput("text")))
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := a.Stream(ctx, partialRequest(0, 2), func(ev llm.StreamEvent) error {
		if ev.Kind != "text" {
			t.Error("unexpected event", ev.Kind)
		}
		cancel()
		return nil
	})
	var partial *llm.PartialError
	var transient *llm.TransientError
	if !errors.Is(err, context.Canceled) || errors.As(err, &partial) || errors.As(err, &transient) || requests.Load() != 1 {
		t.Fatal("cancellation lost or authorized recovery", err, requests.Load())
	}
}
