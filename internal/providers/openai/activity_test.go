package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestInferenceWatchdogExcludesTransportHeartbeats(t *testing.T) {
	for _, heartbeat := range []string{
		": heartbeat\n\n",
		"event: ping\n\n",
		"data: {\"type\":\"ping\"}\n\n",
		"data: {\"type\":\"response.unknown\",\"delta\":\"noise\"}\n\n",
		"data: {\"type\":\"response.in_progress\"}\n\n",
		"data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"missing identity\"}\n\n",
		"x", // Bytes that never form a complete SSE line or event.
	} {
		t.Run(heartbeat, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					fmt.Fprint(w, heartbeat)
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
					}
				}
			}))
			defer server.Close()
			a := idleStreamAdapter(nil)
			a.Client = server.Client()
			a.BaseURL = server.URL
			a.idleTimeout = 100 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			start := time.Now()
			err := a.Stream(ctx, partialRequest(0, 1), func(llm.StreamEvent) error { return nil })
			var idle *idleHTTPError
			if !errors.As(err, &idle) || errors.Is(err, context.Canceled) || ctx.Err() != nil || time.Since(start) > 750*time.Millisecond {
				t.Fatal("heartbeat defeated model watchdog", err, time.Since(start))
			}
		})
	}
}

func TestRecognizedModelActivityKeepsInferenceAlive(t *testing.T) {
	for _, kind := range []string{"reasoning", "text", "tool", "lifecycle", "content part"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(events ...any) { fmt.Fprint(w, sseFrames(events...)); w.(http.Flusher).Flush() }
				send(map[string]any{"type": "response.created", "response": map[string]any{"id": "response-one"}})
				calls := callEvents(0, "item-one", "call-one", "read", "{}")
				if kind == "tool" {
					send(calls[0])
				}
				for range 8 {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(40 * time.Millisecond):
					}
					switch kind {
					case "reasoning":
						send(map[string]any{"type": "response.reasoning_summary_text.delta", "delta": "thinking", "item_id": "reasoning-one", "output_index": 0, "summary_index": 0})
					case "text":
						send(map[string]any{"type": "response.output_text.delta", "delta": "x"})
					case "tool":
						send(map[string]any{"type": "response.function_call_arguments.delta", "delta": " ", "item_id": "item-one", "output_index": 0})
					case "lifecycle":
						send(map[string]any{"type": "response.in_progress", "response": map[string]any{"id": "response-one"}})
					case "content part":
						send(map[string]any{"type": "response.content_part.added", "item_id": "message-one", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})
					}
				}
				if kind == "text" {
					send(messageDone(0, strings.Repeat("x", 8)))
				}
				if kind == "tool" {
					send(calls[1], calls[2], calls[3], calls[4])
				}
				send(map[string]any{"type": "response.completed", "response": map[string]any{"id": "response-one"}})
			}))
			defer server.Close()
			a := idleStreamAdapter(nil)
			a.Client = server.Client()
			a.BaseURL = server.URL
			a.idleTimeout = 160 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start := time.Now()
			err := a.Stream(ctx, partialRequest(0, 1), func(llm.StreamEvent) error { return nil })
			if err != nil || time.Since(start) < 2*a.idleTimeout {
				t.Fatal("recognized progress did not extend model clock", err, time.Since(start))
			}
		})
	}
}

func TestReasoningActivityDoesNotCommitPartialOutput(t *testing.T) {
	x := newResponseExchange(context.Background(), func(context.Context, string) error { return nil }, 75*time.Millisecond)
	body := &stalledIdleBody{ctx: x.ctx, prefix: strings.NewReader(sseFrames(map[string]any{"type": "response.reasoning_text.delta", "delta": "private reasoning", "item_id": "reasoning-one", "output_index": 0}))}
	x.setBody(body)
	committed, err := parseStreamWithWatch("", body, func(llm.StreamEvent) error { t.Error("reasoning became output"); return nil }, x.watch, x.identify)
	err = x.watch.failure(err)
	x.finish(false)
	var idle *idleHTTPError
	if committed || !errors.As(err, &idle) {
		t.Fatal(committed, err)
	}
}

func TestCompletionDisarmsWatchdogBeforeConsumerDelivery(t *testing.T) {
	for _, consumerErr := range []error{nil, errors.New("consumer delivery failed")} {
		var canceled bool
		x := newResponseExchange(context.Background(), func(context.Context, string) error { canceled = true; return nil }, 30*time.Millisecond)
		_, err := parseStreamWithWatch("", strings.NewReader(sseFrames(map[string]any{"type": "response.completed"})), func(llm.StreamEvent) error { time.Sleep(80 * time.Millisecond); return consumerErr }, x.watch, x.identify)
		err = x.watch.failure(err)
		x.finish(err == nil)
		if !errors.Is(err, consumerErr) || canceled || x.ctx.Err() == nil {
			t.Fatal(err, canceled, x.ctx.Err())
		}
	}
}
