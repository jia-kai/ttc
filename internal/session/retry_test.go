package session

import (
	"context"
	"encoding/json"
	"scicode/internal/tool"
	"strings"
	"testing"

	"scicode/internal/provider"
)

func TestMainAndChildRetryMessagesAreInspectableAndNotModelVisible(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	p := &childProvider{}
	p.stream = func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		if err := emit(provider.StreamEvent{Kind: "retry", Retry: &provider.Retry{Attempt: 4, DelayMilliseconds: 1250, Reason: "HTTP 503"}}); err != nil {
			return err
		}
		return emit(provider.StreamEvent{Kind: "text", Text: "Finished"})
	}
	r.Provider = p
	message := provider.Message{Role: "user", Content: "Run"}
	if err := r.Run(&message); err != nil {
		t.Fatal(err)
	}
	request, err := r.Store.StartRequest(r.Current(), "", "main", "coding", r.CurrentSelection())
	if err != nil {
		t.Fatal(err)
	}
	call := provider.ToolCall{ID: "child_retry", Name: "subagent", Arguments: []byte(`{"prompt":"Run child","label":"retry test"}`)}
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
