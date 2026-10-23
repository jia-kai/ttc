package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

func TestBoundaryExactProjectedRequestAndNativeItemsAcrossRetry(t *testing.T) {
	// Whitespace, escape spelling and unknown fields must survive observation.
	items := []string{
		`{ "type":"reasoning", "id":"r1", "encrypted_content":"opaque", "summary":[], "extra":"\u0061" }`,
		`{ "type":"message", "id":"m1", "role":"assistant", "content":[{"type":"output_text","text":"Final"}] }`,
	}
	var observed []llm.BoundaryEvent
	var bodies []string
	requests := 0
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		requests++
		w.Header().Set("Retry-After", "0")
		if requests == 1 {
			w.WriteHeader(503)
			io.WriteString(w, `{"error":{"message":"private-token"}}`)
			return
		}
		io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":"+items[0]+"}\n\n")
		io.WriteString(w, sseFrames(map[string]any{"type": "response.output_text.delta", "delta": "Final"}))
		io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":"+items[1]+"}\n\n")
		io.WriteString(w, sseFrames(map[string]any{"type": "response.completed"}))
	})
	a.tokenSource = func(context.Context) (AccessTokens, error) {
		return AccessTokens{Access: "private-token", AccountID: "private-account"}, nil
	}
	req := partialRequest(1, 3)
	canonical := []llm.Message{{Type: llm.TypeUserInput, Content: "retained <input>", InputTimeMS: 1000, RetainedAtMS: 2000}, {Type: llm.TypeUserInput, Content: "recent"}}
	var err error
	req.Messages, err = contextbuild.ProjectRetainedInputs(canonical)
	if err != nil {
		t.Fatal(err)
	}
	req.Observe = func(event llm.BoundaryEvent) error { observed = append(observed, event); return nil }
	var states []json.RawMessage
	err = a.Stream(context.Background(), req, func(event llm.StreamEvent) error {
		if event.Kind == "state" {
			states = append(states, event.StateItem)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var directions []string
	for _, ev := range observed {
		wantDocument, err := a.InspectBoundary(ev.Direction, ev.Payload)
		if err != nil || ev.Inspection == nil || !reflect.DeepEqual(*ev.Inspection, wantDocument) {
			t.Fatal("missing or mismatched native inspection", ev, err)
		}
		directions = append(directions, ev.Direction)
		if strings.Contains(string(ev.Payload), "private-") {
			t.Fatal("credential leaked", ev)
		}
	}
	if !reflect.DeepEqual(directions, []string{"request", "outcome", "request", "response", "response", "outcome"}) {
		t.Fatal(directions)
	}
	for i, index := range []int{0, 2} {
		if observed[index].Attempt != i+2 || string(observed[index].Payload) != bodies[i] {
			t.Fatal("wire mismatch", observed[index], bodies[i])
		}
	}
	if !strings.Contains(bodies[0], "retained_inputs") || strings.Contains(bodies[0], "retained_at_ms") {
		t.Fatal("projected input not observed", bodies[0])
	}
	for i, index := range []int{3, 4} {
		if observed[index].Attempt != 3 || string(observed[index].Payload) != items[i] || string(states[i]) != items[i] {
			t.Fatal("native item changed", observed[index])
		}
	}
	if string(observed[1].Payload) != `{"status":"failure","category":"http_error","http_status":503}` || string(observed[5].Payload) != `{"status":"success","category":"completed","http_status":200}` {
		t.Fatal(observed)
	}
}

func TestBoundaryNoToolsCapturesDiscardedItems(t *testing.T) {
	for _, recoverReply := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "retry"}[recoverReply], func(t *testing.T) {
			requests := 0
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Retry-After", "0")
				text := "Discarded"
				if requests > 1 {
					text = "Final"
				}
				io.WriteString(w, sseFrames(map[string]any{"type": "response.output_text.delta", "delta": text}, messageDone(0, text)))
				if requests == 1 {
					io.WriteString(w, `data: {"type":"response.failed","response":{"error":{"message":"private-token"}}}`+"\n\n")
					return
				}
				io.WriteString(w, sseFrames(map[string]any{"type": "response.completed"}))
			})
			req := partialRequest(0, 1)
			req.NoTools = true
			if recoverReply {
				req.MaxAttempts = 2
			}
			var observed []llm.BoundaryEvent
			req.Observe = func(ev llm.BoundaryEvent) error { observed = append(observed, ev); return nil }
			text := ""
			states := 0
			err := a.Stream(context.Background(), req, func(ev llm.StreamEvent) error {
				text += ev.Text
				if ev.Kind == "state" {
					states++
				}
				return nil
			})
			if (err == nil) != recoverReply {
				t.Fatal(err)
			}
			wantText := ""
			wantStates := 0
			if recoverReply {
				wantText = "Final"
				wantStates = 1
			}
			if text != wantText || states != wantStates || len(observed) < 3 || observed[1].Direction != "response" || !strings.Contains(string(observed[1].Payload), "Discarded") || !strings.Contains(string(observed[2].Payload), `"status":"failure"`) {
				t.Fatal(text, states, observed)
			}
		})
	}
}

func TestBoundaryCallbackFailuresNeverRetry(t *testing.T) {
	for _, direction := range []string{"request", "response", "outcome"} {
		t.Run(direction, func(t *testing.T) {
			requests := 0
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				io.WriteString(w, sseFrames(map[string]any{"type": "response.output_text.delta", "delta": "Final"}, messageDone(0, "Final"), map[string]any{"type": "response.completed"}))
			})
			failure := &llm.PartialError{Err: errors.New("capture unavailable"), Retry: llm.Retry{Attempt: 2, MaxAttempts: 3}}
			req := partialRequest(0, 3)
			req.Observe = func(ev llm.BoundaryEvent) error {
				if ev.Direction == direction {
					return failure
				}
				return nil
			}
			retries := 0
			err := a.Stream(context.Background(), req, func(ev llm.StreamEvent) error {
				if ev.Kind == "retry" {
					retries++
				}
				return nil
			})
			var partial *llm.PartialError
			wantRequests := 1
			if direction == "request" {
				wantRequests = 0
			}
			if !errors.Is(err, failure) || errors.As(err, &partial) || requests != wantRequests || retries != 0 {
				t.Fatal(err, requests, retries)
			}
		})
	}
}

func TestBoundaryMalformedNativeItemPreservesRetrySemantics(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_output", true: "after_output"}[committed], func(t *testing.T) {
			for _, observe := range []bool{false, true} {
				t.Run(map[bool]string{false: "unobserved", true: "observed"}[observe], func(t *testing.T) {
					const malformed = `[ ]`
					var bodies []string
					a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
						body, _ := io.ReadAll(r.Body)
						bodies = append(bodies, string(body))
						w.Header().Set("Retry-After", "0")
						if committed {
							io.WriteString(w, sseFrames(partialOutput("text")))
						}
						io.WriteString(w, `data: {"type":"response.output_item.done","output_index":0,"item":`+malformed+"}\n\n")
					})
					req := partialRequest(0, 2)
					var captured []llm.BoundaryEvent
					if observe {
						req.Observe = func(event llm.BoundaryEvent) error {
							captured = append(captured, event)
							return nil
						}
					}
					err := a.Stream(context.Background(), req, func(llm.StreamEvent) error { return nil })
					var partial *llm.PartialError
					var transient *llm.TransientError
					var callback *callbackError
					wantRequests := 2
					if committed {
						wantRequests = 1
					}
					if len(bodies) != wantRequests || errors.As(err, &partial) != committed || !errors.As(err, &transient) || errors.As(err, &callback) {
						t.Fatalf("observation changed malformed-item handling: requests=%d partial=%v error=%T %v", len(bodies), partial, err, err)
					}
					requests, responses := 0, 0
					for _, event := range captured {
						switch event.Direction {
						case "request":
							if string(event.Payload) != bodies[requests] {
								t.Fatal("request capture changed exact wire body")
							}
							requests++
						case "response":
							responses++
							if string(event.Payload) != malformed || event.Inspection == nil || event.Inspection.Sections[0].Title != "Inspection unavailable" {
								t.Fatal("malformed raw item or unavailable document lost", event)
							}
						case "outcome":
							if strings.Contains(string(event.Payload), "callback_error") {
								t.Fatal("protocol error mislabeled local callback failure", event)
							}
						}
					}
					if observe && (requests != wantRequests || responses != wantRequests) {
						t.Fatal("missing malformed boundary captures", captured)
					}
				})
			}
		})
	}
}

func TestBoundarySafeTransportAndCancellationOutcomes(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport", true: "canceled"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := NewAdapter(Config{Client: &http.Client{Transport: retryTransport(func(*http.Request) (*http.Response, error) {
				if canceled {
					cancel()
				}
				return nil, errors.New("private-token private URL")
			})}, BaseURL: "https://example.invalid", TokenSource: func(context.Context) (AccessTokens, error) {
				return AccessTokens{Access: "private-token", AccountID: "private-account"}, nil
			}})
			req := partialRequest(0, 1)
			var observed []llm.BoundaryEvent
			req.Observe = func(ev llm.BoundaryEvent) error { observed = append(observed, ev); return nil }
			err := a.Stream(ctx, req, func(llm.StreamEvent) error { return nil })
			category := "transport_error"
			if canceled {
				category = "canceled"
			}
			if err == nil || len(observed) != 2 || string(observed[1].Payload) != `{"status":"failure","category":"`+category+`"}` {
				t.Fatal(err, observed)
			}
		})
	}
}

func TestBoundaryCaptureDoesNotMutateRequestOrReplay(t *testing.T) {
	var body string
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, sseFrames(map[string]any{"type": "response.output_text.delta", "delta": "Final"}, messageDone(0, "Final"), map[string]any{"type": "response.completed"}))
	})
	req := partialRequest(0, 1)
	req.Observe = func(ev llm.BoundaryEvent) error {
		for i := range ev.Payload {
			ev.Payload[i] = 'x'
		}
		return nil
	}
	states := 0
	err := a.Stream(context.Background(), req, func(ev llm.StreamEvent) error {
		if ev.Kind == "state" {
			states++
			if !json.Valid(ev.StateItem) {
				t.Fatal("observer mutated state")
			}
		}
		return nil
	})
	if err != nil || !json.Valid([]byte(body)) || states != 1 {
		t.Fatal(err, body, states)
	}
}

func TestBoundaryInvalidHeadersRemainUnobserved(t *testing.T) {
	requests, captures := 0, 0
	a := adapterFixture(t, func(http.ResponseWriter, *http.Request) { requests++ })
	a.tokenSource = func(context.Context) (AccessTokens, error) {
		return AccessTokens{Access: "private\ninvalid", AccountID: "account"}, nil
	}
	req := partialRequest(0, 3)
	req.Observe = func(llm.BoundaryEvent) error { captures++; return nil }
	err := a.Stream(context.Background(), req, func(llm.StreamEvent) error { return nil })
	if err == nil || requests != 0 || captures != 0 {
		t.Fatal(err, requests, captures)
	}
}

func TestBoundaryOutcomeFailureAbortsUpstreamRetry(t *testing.T) {
	requests, retries := 0, 0
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(503)
	})
	failure := &llm.TransientError{Err: errors.New("capture unavailable")}
	req := partialRequest(0, 3)
	req.Observe = func(ev llm.BoundaryEvent) error {
		if ev.Direction == "outcome" {
			return failure
		}
		return nil
	}
	err := a.Stream(context.Background(), req, func(ev llm.StreamEvent) error {
		if ev.Kind == "retry" {
			retries++
		}
		return nil
	})
	var transient *llm.TransientError
	if !errors.Is(err, failure) || errors.As(err, &transient) || requests != 1 || retries != 0 {
		t.Fatal(err, requests, retries)
	}
}

func TestBoundaryIncompleteOutputHasFailureOutcomeWithoutSyntheticItem(t *testing.T) {
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sseFrames(map[string]any{"type": "response.output_text.delta", "delta": "partial"}))
	})
	req := partialRequest(0, 1)
	var observed []llm.BoundaryEvent
	req.Observe = func(ev llm.BoundaryEvent) error { observed = append(observed, ev); return nil }
	text := ""
	err := a.Stream(context.Background(), req, func(ev llm.StreamEvent) error { text += ev.Text; return nil })
	if err == nil || text != "partial" || len(observed) != 2 || observed[0].Direction != "request" || observed[1].Direction != "outcome" || string(observed[1].Payload) != `{"status":"failure","category":"response_error","http_status":200}` {
		t.Fatal(err, text, observed)
	}
}

func TestBoundarySlowObserverExcludesLocalTimeFromInactivity(t *testing.T) {
	requests := 0
	var transportContext context.Context
	a := idleStreamAdapter(retryTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		transportContext = r.Context()
		return idleTestResponse(io.NopCloser(strings.NewReader(sseFrames(
			map[string]any{"type": "response.output_text.delta", "delta": "Final"},
			messageDone(0, "Final"), map[string]any{"type": "response.completed"})))), nil
	}))
	req := partialRequest(0, 2)
	observed := 0
	req.Observe = func(ev llm.BoundaryEvent) error {
		if ev.Direction == "response" {
			observed++
			time.Sleep(3 * a.idleTimeout)
			if err := transportContext.Err(); err != nil {
				t.Errorf("local capture canceled transport: %v", err)
			}
		}
		return nil
	}
	retries, completed := 0, false
	err := a.Stream(context.Background(), req, func(ev llm.StreamEvent) error {
		if ev.Kind == "retry" {
			retries++
		}
		completed = completed || ev.Kind == "completed"
		return nil
	})
	if err != nil || requests != 1 || retries != 0 || observed != 1 || !completed {
		t.Fatal(err, requests, retries, observed, completed)
	}
}

func TestBoundaryObserverPausePreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var transportContext context.Context
	a := idleStreamAdapter(retryTransport(func(r *http.Request) (*http.Response, error) {
		transportContext = r.Context()
		return idleTestResponse(io.NopCloser(strings.NewReader(sseFrames(messageDone(0, "Final"))))), nil
	}))
	req := partialRequest(0, 2)
	req.Observe = func(ev llm.BoundaryEvent) error {
		if ev.Direction == "response" {
			close(entered)
			<-release
		}
		return nil
	}
	result := make(chan error, 1)
	go func() { result <- a.Stream(ctx, req, func(llm.StreamEvent) error { return nil }) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("observer not reached")
	}
	cancel()
	select {
	case <-transportContext.Done():
	case <-time.After(time.Second):
		close(release)
		t.Fatal("paused watchdog blocked transport cancellation")
	}
	close(release)
	select {
	case err := <-result:
		var idle *idleHTTPError
		if !errors.Is(err, context.Canceled) || errors.As(err, &idle) {
			t.Fatal("cancellation lost during observer", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not return after observer release")
	}
}

func TestBoundaryObserverResumesInactivityForRemainingStream(t *testing.T) {
	a := idleStreamAdapter(retryTransport(func(r *http.Request) (*http.Response, error) {
		return idleTestResponse(&stalledIdleBody{
			ctx: r.Context(), prefix: strings.NewReader(sseFrames(messageDone(0, "Final"))),
		}), nil
	}))
	req := partialRequest(0, 1)
	observed := false
	req.Observe = func(ev llm.BoundaryEvent) error {
		if ev.Direction == "response" {
			observed = true
			time.Sleep(3 * a.idleTimeout)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := a.Stream(ctx, req, func(llm.StreamEvent) error { return nil })
	var idle *idleHTTPError
	if !observed || !errors.As(err, &idle) || ctx.Err() != nil {
		t.Fatal("observer disabled remaining upstream inactivity timeout", observed, err)
	}
}
