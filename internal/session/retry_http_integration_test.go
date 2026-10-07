package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/providers/openai"
)

// Use the real adapter and HTTP parser, with no real credentials, TCP sockets or
// external service. Every failed upstream attempt must survive either as a retry notice
// or as the terminal request error, even when a later attempt succeeds.
func TestHTTPMockUnknownErrorsPersistAcrossBoundedRetries(t *testing.T) {
	for _, shape := range []string{"nested", "bare"} {
		for _, actor := range []string{"main", "main/child_retry_http"} {
			for _, recover := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/recover=%t", shape, actor, recover), func(t *testing.T) {
					r, _ := runtimeFixture(t, nil)
					var attempts atomic.Int32
					installRetryHTTPAdapter(t, r, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						attempt := int(attempts.Add(1))
						w.Header().Set("Retry-After", "0")
						if recover && attempt == llm.DefaultMaxAttempts {
							streamMockResponse(w, "retry", attempt, nil, "Recovered answer")
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						w.Header().Set("x-request-id", fmt.Sprintf("mock-upstream-%d", attempt))
						payload := `{"type":"error"}`
						if shape == "nested" {
							payload = fmt.Sprintf(`{"type":"error","error":{"code":"unknown_backend_fault","message":"Backend failure %d"}}`, attempt)
						}
						fmt.Fprintf(w, "data: %s\n\n", payload)
					}))
					err := partialRetryRunActor(t, r, actor, io.Discard)
					if recover {
						if err != nil {
							t.Fatal("recovery failed", err)
						}
					} else {
						var upstream *llm.TransientError
						if err == nil || !errors.As(err, &upstream) {
							t.Fatal("exhaustion must leave conversation context usable", err)
						}
					}
					if attempts.Load() != llm.DefaultMaxAttempts {
						t.Fatal("incorrect resolved attempt limit", attempts.Load())
					}
					request := assertHTTPRetryHistory(t, r, actor, "coding", shape)
					var status, recordedError string
					if err := r.Store.DB.QueryRow("SELECT status,coalesce(json_extract(attempts_json,'$[0].error'),'') FROM model_requests WHERE id=?", request).Scan(&status, &recordedError); err != nil {
						t.Fatal(err)
					}
					if recover {
						if status != "completed" || recordedError != "" {
							t.Fatal("successful request retained a terminal failure", status, recordedError)
						}
					} else {
						if status != "failed" || recordedError != err.Error() {
							t.Fatal("final diagnostic absent from request attempts", status, recordedError, err)
						}
						if !strings.Contains(recordedError, "mock-upstream-3") {
							t.Fatal("final upstream request identity lost", recordedError)
						}
						if shape == "nested" && (!strings.Contains(recordedError, "unknown_backend_fault") || !strings.Contains(recordedError, "Backend failure 3")) {
							t.Fatal("nested final diagnostic lost", recordedError)
						}
						if shape == "bare" && (!strings.Contains(recordedError, "error details missing") || !strings.Contains(recordedError, "shape=")) {
							t.Fatal("bare final structural diagnostic lost", recordedError)
						}
						if actor == "main" {
							var turnError string
							if err := r.Store.DB.QueryRow("SELECT json_extract(content_json,'$.error') FROM entries WHERE actor_id=? AND json_extract(content_json,'$.type')='turn_end'", actor).Scan(&turnError); err != nil || turnError != recordedError {
								t.Fatal("terminal history lost request diagnostic", turnError, err)
							}
						}
					}
					messages, err := r.Store.Messages(r.Current())
					if err != nil {
						t.Fatal(err)
					}
					for _, message := range messages {
						if strings.Contains(message.Content, "Backend failure") || strings.Contains(message.Content, "mock-upstream") || strings.Contains(message.Content, "Retrying") {
							t.Fatal("upstream diagnostics became model input", message)
						}
					}
				})
			}
		}
	}
}

func installRetryHTTPAdapter(t *testing.T, r *Runtime, handler http.Handler) {
	t.Helper()
	r.Provider = openai.NewAdapter(openai.Config{
		Client: mockHTTPClient(t, handler), BaseURL: "http://mock.invalid",
		TokenSource: func(context.Context) (openai.AccessTokens, error) {
			return openai.AccessTokens{Access: "mock-token", AccountID: "mock-account"}, nil
		},
	})
	r.selection.Provider = "openai"
}

func assertHTTPRetryHistory(t *testing.T, r *Runtime, actor, purpose, shape string) int64 {
	t.Helper()
	entries, err := r.Store.Branch(r.Current(), 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	var request int64
	for _, entry := range entries {
		var notice struct {
			Type      string    `json:"type"`
			RequestID int64     `json:"request_id"`
			Retry     llm.Retry `json:"retry"`
		}
		if err := json.Unmarshal(entry.Content, &notice); err != nil || notice.Type != "model_retry" {
			continue
		}
		count++
		if entry.Actor != actor || entry.Visible || notice.Retry.Attempt != count+1 || notice.Retry.MaxAttempts != llm.DefaultMaxAttempts || notice.Retry.DelayMilliseconds != 0 {
			t.Fatal("incorrect persisted retry contract", entry, notice)
		}
		var requestActor, requestPurpose string
		if err := r.Store.DB.QueryRow("SELECT actor_id,purpose FROM model_requests WHERE id=?", notice.RequestID).Scan(&requestActor, &requestPurpose); err != nil || requestActor != actor || requestPurpose != purpose {
			t.Fatal("retry lost originating request", requestActor, requestPurpose, err)
		}
		if request != 0 && request != notice.RequestID {
			t.Fatal("pre-output retries unexpectedly admitted another request")
		}
		request = notice.RequestID
		inspected, err := r.Store.Inspect(entry)
		if err != nil || !strings.Contains(inspected, "model_retry") || !strings.Contains(inspected, fmt.Sprintf("mock-upstream-%d", count)) {
			t.Fatal("retry diagnostics not inspectable", inspected, err)
		}
		if shape == "bare" && (!strings.Contains(inspected, "error details missing") || !strings.Contains(inspected, "shape=")) {
			t.Fatal("bare terminal error lost its structural diagnostic", inspected)
		}
		if shape == "nested" && (!strings.Contains(inspected, "unknown_backend_fault") || !strings.Contains(inspected, fmt.Sprintf("Backend failure %d", count))) {
			t.Fatal("nested retry diagnostic lost before success/exhaustion", inspected)
		}
	}
	if count != llm.DefaultMaxAttempts-1 {
		t.Fatal("missing failed upstream attempt notices", count)
	}
	return request
}

func TestHTTPMockNamingAndCompactionShareDefaultRetries(t *testing.T) {
	for _, purpose := range []string{"naming", "compaction"} {
		t.Run(purpose, func(t *testing.T) {
			r, events := runtimeFixture(t, nil)
			seedRuntime(t, r, "Summarize this research task")
			var attempts atomic.Int32
			installRetryHTTPAdapter(t, r, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				attempt := int(attempts.Add(1))
				w.Header().Set("Retry-After", "0")
				if attempt < llm.DefaultMaxAttempts {
					w.Header().Set("x-request-id", fmt.Sprintf("mock-upstream-%d", attempt))
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprintf(w, `{"error":{"code":"unknown_backend_fault","message":"Backend failure %d"}}`, attempt)
					return
				}
				streamMockResponse(w, purpose, attempt, nil, "Recovered Research Session")
			}))
			if purpose == "naming" {
				r.name(context.Background(), r.Current(), r.CurrentSelection(), "", llm.Message{Role: "user", Content: "Research task"}, llm.Message{Role: "assistant", Content: "Research completed"})
				session, err := r.Store.Session(r.Current())
				if err != nil || session.Name != "Recovered Research Session" {
					t.Fatal("naming did not recover with the unified default", session.Name, err)
				}
			} else {
				text, err := r.summarize(context.Background(), "main", "", r.CurrentSelection(), "Research transcript", "")
				if err != nil || text != "Recovered Research Session" {
					t.Fatal("compaction summary did not recover", text, err)
				}
			}
			if attempts.Load() != llm.DefaultMaxAttempts {
				t.Fatal("naming/compaction attempt limit differs", attempts.Load())
			}
			assertHTTPRetryHistory(t, r, "main", purpose, "nested")
			for len(events) > 0 {
				event := <-events
				if strings.Contains(event.Text, "Retrying") && (event.Retry != nil) != (purpose != "naming") {
					t.Fatal("background naming retry changed foreground activity", event)
				}
			}
		})
	}
}

func TestHTTPMockPrivatePartialFailuresPersistAfterExhaustion(t *testing.T) {
	for _, purpose := range []string{"naming", "compaction"} {
		t.Run(purpose, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Summarize this research task")
			attempts := 0
			installRetryHTTPAdapter(t, r, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				attempts++
				w.Header().Set("Retry-After", "0")
				w.Header().Set("x-request-id", fmt.Sprintf("mock-upstream-%d", attempts))
				io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Discarded private reply\"}\n\n")
				fmt.Fprintf(w, "data: {\"type\":\"error\",\"error\":{\"code\":\"unknown_backend_fault\",\"message\":\"Backend failure %d\"}}\n\n", attempts)
			}))
			if purpose == "naming" {
				r.name(context.Background(), r.Current(), r.CurrentSelection(), "", llm.Message{Role: "user", Content: "Research task"}, llm.Message{Role: "assistant", Content: "Research completed"})
			} else {
				text, err := r.summarize(context.Background(), "main", "", r.CurrentSelection(), "Research transcript", "")
				if err == nil || text != "" || !recoverableCompaction(err) {
					t.Fatal("private reply exhaustion invalidated usable context or leaked text", text, err)
				}
			}
			if attempts != llm.DefaultMaxAttempts {
				t.Fatal("private output prevented bounded upstream retries", attempts)
			}
			request := assertHTTPRetryHistory(t, r, "main", purpose, "nested")
			var status, diagnostic string
			if err := r.Store.DB.QueryRow("SELECT status,json_extract(attempts_json,'$[0].error') FROM model_requests WHERE id=?", request).Scan(&status, &diagnostic); err != nil || status != "failed" || !strings.Contains(diagnostic, "upstream attempt 3/3") || !strings.Contains(diagnostic, "Backend failure 3") {
				t.Fatal("private request final failure was not recorded", status, diagnostic, err)
			}
			exact, err := r.Store.TranscriptJSONL(r.Current(), 0)
			if err != nil || !strings.Contains(string(exact), "Backend failure 3") || strings.Contains(string(exact), "Discarded private reply") {
				t.Fatal("export lost final diagnostic or leaked failed private text", string(exact), err)
			}
		})
	}
}
