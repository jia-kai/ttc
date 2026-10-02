package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"scicode/internal/provider"
	"scicode/internal/tool"
)

func childInvocation(t *testing.T, r *Runtime, callID, arguments string) map[string]any {
	t.Helper()
	record := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: r.Current(), Actor: "main", CallID: callID}, "subagent", []byte(arguments))
	var result map[string]any
	if err := json.Unmarshal(record.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestChildIdleFollowupRetainsContextAndDistinctAssignments(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	var conversations []string
	r.Provider = &childProvider{stream: func(ctx context.Context, request provider.Request, emit func(provider.StreamEvent) error) error {
		conversations = append(conversations, request.ConversationID)
		if len(conversations) == 2 {
			found := false
			for _, m := range request.Messages {
				found = found || m.Role == "assistant" && m.Content == "First complete answer"
			}
			if !found {
				t.Error("follow-up lost isolated context")
			}
		}
		return emit(provider.StreamEvent{Kind: "text", Text: "First complete answer"})
	}}
	turn, ids := batchIntents(t, r, "main", []provider.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"first","label":"research"}`)}})
	first := childInvocation(t, r, ids[0], `{"persistent":true,"prompt":"first","label":"research"}`)
	if first["ok"] != true || first["finish_event_seq"] == nil {
		t.Fatal(first)
	}
	childID := first["child_id"].(string)
	args, _ := json.Marshal(map[string]any{"persistent": true, "prompt": "follow up", "child_id": childID})
	second := childInvocation(t, r, ids[0], string(args))
	if second["ok"] != true || second["child_id"] != childID || second["job_id"] == first["job_id"] || second["child_turn_id"] == first["child_turn_id"] || second["finish_event_seq"] == first["finish_event_seq"] {
		t.Fatal(first, second)
	}
	if len(conversations) != 2 || conversations[0] != conversations[1] {
		t.Fatal(conversations)
	}
	if r.HasNotifications() {
		t.Fatal("foreground completion duplicated as notification")
	}
	views := r.ChildViews("main")
	if len(views) != 1 || views[0].State != "idle" {
		t.Fatal(views)
	}
	var finished int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='child_turn_finished'").Scan(&finished); err != nil || finished != 2 {
		t.Fatal(finished, err)
	}
	if _, err := r.StopChild(context.Background(), "main", childID); err != nil {
		t.Fatal(err)
	}
	if len(r.ChildViews("main")) != 0 {
		t.Fatal("closed context retained")
	}
	if result := childInvocation(t, r, ids[0], string(args)); result["ok"] == true {
		t.Fatal("followed up closed context", result)
	}
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
}

func TestChildBusyFollowupAndBackgroundCancellationNotifyOnce(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	started := make(chan struct{})
	r.Provider = &childProvider{stream: func(ctx context.Context, request provider.Request, emit func(provider.StreamEvent) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	turn, ids := batchIntents(t, r, "main", []provider.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"first","label":"research","background":true}`)}})
	first := childInvocation(t, r, ids[0], `{"persistent":true,"prompt":"first","label":"research","background":true}`)
	if first["ok"] != true {
		t.Fatal(first)
	}
	receive(t, started)
	childID := first["child_id"].(string)
	args, _ := json.Marshal(map[string]any{"persistent": true, "prompt": "follow up", "child_id": childID})
	result := childInvocation(t, r, ids[0], string(args))
	encoded, _ := json.Marshal(result)
	if result["ok"] == true || !strings.Contains(string(encoded), "child_busy") {
		t.Fatal(result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := r.StopChild(ctx, "main", childID); err != nil {
		t.Fatal(err)
	}
	if !r.HasNotifications() {
		t.Fatal("background cancellation did not notify main")
	}
	r.orderMu.Lock()
	count, content := len(r.notifications), r.notifications[0].Content
	r.orderMu.Unlock()
	if count != 1 || !strings.Contains(content, "child_turn_finished") || !strings.Contains(content, "cancelled") {
		t.Fatal(count, content)
	}
	if len(r.ChildViews("main")) != 0 {
		t.Fatal("cancelled child remained reusable")
	}
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
}

func TestChildFailureClosesContextAndKeepsImmutableReply(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	r.Provider = &childProvider{stream: func(ctx context.Context, request provider.Request, emit func(provider.StreamEvent) error) error {
		if err := emit(provider.StreamEvent{Kind: "text", Text: "Partial evidence"}); err != nil {
			return err
		}
		return errors.New("synthetic failure")
	}}
	_, ids := batchIntents(t, r, "main", []provider.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"first","label":"research"}`)}})
	result := childInvocation(t, r, ids[0], `{"persistent":true,"prompt":"first","label":"research"}`)
	if result["status"] != "failed" || result["result_entry_id"] == nil || len(r.ChildViews("main")) != 0 {
		t.Fatal(result)
	}
	entry, err := r.Store.Entry(int64(result["result_entry_id"].(float64)))
	if err != nil || !strings.Contains(string(entry.Content), "Partial evidence") {
		t.Fatal(entry, err)
	}
}
