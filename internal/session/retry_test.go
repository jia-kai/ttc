package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"ttc/internal/tool"

	"ttc/internal/llm"
)

func TestMainAndChildRetryMessagesAreInspectableAndNotModelVisible(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	p := &childProvider{}
	p.stream = func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if err := emit(llm.StreamEvent{Kind: "retry", Retry: &llm.Retry{Attempt: 4, MaxAttempts: 4, DelayMilliseconds: 1250, Reason: "HTTP 503"}}); err != nil {
			return err
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Finished"})
	}
	r.Provider = p
	message := llm.Message{Role: "user", Content: "Run"}
	if err := r.Run(&message); err != nil {
		t.Fatal(err)
	}
	request, err := r.Store.StartRequest(r.Current(), "", "main", "coding", r.CurrentSelection())
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{ID: "child_retry", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"Run child","label":"retry test"}`)}
	id, err := r.Store.CallIntent(r.Current(), "", "main", request, call)
	if err != nil {
		t.Fatal(err)
	}
	result := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: r.Current(), Actor: "main", CallID: id}, call.Name, call.Arguments)
	if !strings.Contains(string(result.Result), `"ok":true`) {
		t.Fatal(result)
	}
	count := 0
	for len(events) > 0 {
		event := <-events
		if event.Kind != "status" || !strings.Contains(event.Text, "Retrying") {
			continue
		}
		count++
		entry, err := r.Store.Entry(event.EntryID)
		if err != nil || entry.Visible || entry.SessionID != event.SessionID {
			t.Fatal(entry, err)
		}
		inspected, err := r.Store.Inspect(entry)
		if err != nil || !strings.Contains(inspected, "model_retry") || !strings.Contains(inspected, "1250") {
			t.Fatal(inspected, err)
		}
		var payload struct {
			RequestID int64 `json:"request_id"`
		}
		if err := json.Unmarshal(entry.Content, &payload); err != nil {
			t.Fatal(err)
		}
		var actor string
		if err := r.Store.DB.QueryRow("SELECT actor_id FROM model_requests WHERE id=?", payload.RequestID).Scan(&actor); err != nil || actor != entry.Actor {
			t.Fatal(actor, entry.Actor, err)
		}
	}
	if count != 2 {
		t.Fatal("missing main/child notice", count)
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if strings.Contains(message.Content, "Retrying") {
			t.Fatal("retry became model input", message)
		}
	}
}

func TestRetryActivityExcludesBackgroundNaming(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	seedRuntime(t, r, "Name this session")
	for _, purpose := range []string{"coding", "compaction", "naming"} {
		request, err := r.Store.StartRequest(r.Current(), "", "main", purpose, r.CurrentSelection())
		if err != nil {
			t.Fatal(err)
		}
		retry := llm.Retry{Attempt: 2, MaxAttempts: llm.DefaultMaxAttempts, DelayMilliseconds: 1000, Reason: "HTTP 503"}
		if err := r.retryNotice("", "main", request, purpose, &retry); err != nil {
			t.Fatal(err)
		}
		event := <-events
		if event.Kind != "status" || (event.Retry != nil) != (purpose != "naming") {
			t.Fatalf("purpose %s: %#v", purpose, event)
		}
		if event.Retry != nil {
			retry.DelayMilliseconds = 7
			if event.Retry.DelayMilliseconds != 1000 {
				t.Fatal("retry UI metadata aliases provider-owned memory")
			}
		}
	}
}

func TestMainFailureDetailsPersistWithoutBecomingModelInput(t *testing.T) {
	for _, failure := range []error{errors.New("Responses stream interrupted before completion"), context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			p := &childProvider{}
			p.stream = func(context.Context, llm.Request, func(llm.StreamEvent) error) error { return failure }
			r.Provider = p
			message := llm.Message{Role: "user", Content: "Run"}
			if err := r.Run(&message); !errors.Is(err, failure) {
				t.Fatal("lost original failure", err)
			}
			var requestError, turnError string
			if err := r.Store.DB.QueryRow("SELECT json_extract(attempts_json,'$[0].error') FROM model_requests WHERE session_id=? AND purpose='coding'", r.Current()).Scan(&requestError); err != nil {
				t.Fatal(err)
			}
			if err := r.Store.DB.QueryRow("SELECT json_extract(content_json,'$.error') FROM entries WHERE session_id=? AND json_extract(content_json,'$.type')='turn_end' AND model_visible=0", r.Current()).Scan(&turnError); err != nil {
				t.Fatal(err)
			}
			if requestError != failure.Error() || turnError != failure.Error() {
				t.Fatal("failure details not saved", requestError, turnError)
			}
			messages, err := r.Store.Messages(r.Current())
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range messages {
				if strings.Contains(message.Content, failure.Error()) {
					t.Fatal("diagnostic entered model context", message)
				}
			}
		})
	}
}
