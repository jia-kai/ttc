package session

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"ttc/internal/historytest"
	"ttc/internal/llm"
)

func TestContextLengthCompactionClassificationPreservesJoinedFailures(t *testing.T) {
	capacity := &llm.ContextLengthError{Err: errors.New("structured rejection")}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"direct", capacity, true},
		{"wrapped", fmt.Errorf("request: %w", capacity), true},
		{"recoverable join", errors.Join(capacity, &llm.TransientError{Err: errors.New("unavailable")}), true},
		{"broken persistence", errors.Join(capacity, errors.New("corrupt history")), false},
		{"wrapped broken persistence", fmt.Errorf("save: %w", errors.Join(capacity, errors.New("corrupt history"))), false},
		{"untyped code", errors.New("context_length_exceeded"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recoverableCompaction(tc.err); got != tc.want {
				t.Fatalf("recoverable=%t want %t", got, tc.want)
			}
		})
	}
}

func TestHTTPContextLengthPartialCodingPreservesEvidenceWithoutContinuation(t *testing.T) {
	for _, actor := range []string{"main", "main/child_capacity"} {
		t.Run(actor, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			var requests atomic.Int32
			installRetryHTTPAdapter(t, r, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests.Add(1)
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Preserved partial capacity evidence\"}\n\n")
				fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded\"}}}\n\n")
			}))
			err := partialRetryRunActor(t, r, actor, io.Discard)
			var capacity *llm.ContextLengthError
			var partial *llm.PartialError
			if !errors.As(err, &capacity) || errors.As(err, &partial) || requests.Load() != 1 || r.checkContext(false) != nil {
				t.Fatal("partial context rejection retried or invalidated context", err, requests.Load())
			}
			var content, status, diagnostic string
			if err := historytest.DB(t, r.Store).QueryRow(`SELECT json_extract(content_json,'$.content') FROM entries WHERE actor_id=? AND kind='message' AND role='assistant' ORDER BY id DESC LIMIT 1`, actor).Scan(&content); err != nil || content != "Preserved partial capacity evidence" {
				t.Fatal("partial evidence was lost", content, err)
			}
			if err := historytest.DB(t, r.Store).QueryRow(`SELECT o.status,json_extract(e.content_json,'$.attempts[0].error') FROM model_requests r JOIN request_observations o ON o.request_id=r.id JOIN entries e ON e.id=o.completion_entry_id WHERE r.actor_id=? ORDER BY o.completion_entry_id DESC LIMIT 1`, actor).Scan(&status, &diagnostic); err != nil || status != "failed" || !strings.Contains(diagnostic, "context_length_exceeded") {
				t.Fatal("failed request diagnostic was lost", status, diagnostic, err)
			}
			var automatic int
			if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type') IN ('model_retry','compaction_started')`).Scan(&automatic); err != nil || automatic != 0 {
				t.Fatal("capacity rejection triggered automatic retry or compaction", automatic, err)
			}
			if actor == "main" {
				installRetryHTTPAdapter(t, r, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					requests.Add(1)
					streamMockResponse(w, "explicit-retry", 2, nil, "Explicit retry succeeded")
				}))
				if _, err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Retry explicitly"}); err != nil || requests.Load() != 2 {
					t.Fatal("explicit recovery was blocked", err, requests.Load())
				}
			}
		})
	}
}
