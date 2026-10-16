package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"ttc/internal/historytest"

	"ttc/internal/llm"
	"ttc/internal/tool"
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

func requireChildLifecycle(t *testing.T, value any, status, state string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if result["assignment_status"] != status || result["child_state"] != state || result["status"] != nil || result["state"] != nil {
		t.Fatalf("ambiguous child lifecycle: %s; want assignment=%s child=%s", encoded, status, state)
	}
}

func TestChildIdleFollowupRetainsContextAndDistinctAssignments(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	var conversations []string
	r.Provider = &childProvider{stream: func(ctx context.Context, request llm.Request, emit func(llm.StreamEvent) error) error {
		conversations = append(conversations, request.ConversationID)
		if len(conversations) == 2 {
			found := false
			for _, m := range request.Messages {
				found = found || m.Type == llm.TypeAssistant && m.Content == "First complete answer"
			}
			if !found {
				t.Error("follow-up lost isolated context")
			}
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "First complete answer"})
	}}
	turn, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"first","label":"research"}`)}})
	first := childInvocation(t, r, ids[0], `{"persistent":true,"prompt":"first","label":"research"}`)
	if first["ok"] != true || first["finish_event_seq"] == nil {
		t.Fatal(first)
	}
	requireChildLifecycle(t, first, "completed", "idle")
	// Generic job tools expose scheduler status even for subagent jobs. Their
	// child-context listing, unlike the jobs collection, uses lifecycle fields.
	listing := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: r.Current(), Actor: "main"}, "job_list", []byte(`{"state":"all"}`))
	var listed struct {
		Jobs     []map[string]any `json:"jobs"`
		Children []tool.ChildView `json:"children"`
	}
	if err := json.Unmarshal(listing.Result, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Jobs) != 1 || listed.Jobs[0]["status"] != "completed" || listed.Jobs[0]["assignment_status"] != nil || len(listed.Children) != 1 {
		t.Fatal("generic scheduler contract changed", string(listing.Result))
	}
	requireChildLifecycle(t, listed.Children[0], "completed", "idle")
	childID := first["child_id"].(string)
	args, _ := json.Marshal(map[string]any{"persistent": true, "prompt": "follow up", "child_id": childID})
	second := childInvocation(t, r, ids[0], string(args))
	if second["ok"] != true || second["child_id"] != childID || second["job_id"] == first["job_id"] || second["child_turn_id"] == first["child_turn_id"] || second["finish_event_seq"] == first["finish_event_seq"] {
		t.Fatal(first, second)
	}
	requireChildLifecycle(t, second, "completed", "idle")
	if len(conversations) != 2 || conversations[0] != conversations[1] {
		t.Fatal(conversations)
	}
	if r.HasNotifications() {
		t.Fatal("foreground completion duplicated as notification")
	}
	views := r.ChildViews("main")
	if len(views) != 1 || views[0].ChildState != "idle" {
		t.Fatal(views)
	}
	requireChildLifecycle(t, views[0], "completed", "idle")
	var finished int
	if err := historytest.DB(t, r.Store).QueryRow("SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='child_turn_finished'").Scan(&finished); err != nil || finished != 2 {
		t.Fatal(finished, err)
	}
	closed, err := r.StopChild(context.Background(), "main", childID)
	if err != nil {
		t.Fatal(err)
	}
	requireChildLifecycle(t, closed, "completed", "closed")
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
	r.Provider = &childProvider{stream: func(ctx context.Context, request llm.Request, emit func(llm.StreamEvent) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	turn, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"first","label":"research","background":true}`)}})
	first := childInvocation(t, r, ids[0], `{"persistent":true,"prompt":"first","label":"research","background":true}`)
	if first["ok"] != true {
		t.Fatal(first)
	}
	requireChildLifecycle(t, first, "running", "running")
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
	closed, err := r.StopChild(ctx, "main", childID)
	if err != nil {
		t.Fatal(err)
	}
	requireChildLifecycle(t, closed, "cancelled", "closed")
	if !r.HasNotifications() {
		t.Fatal("background cancellation did not notify main")
	}
	r.orderMu.Lock()
	count, content := len(r.notifications), r.notifications[0].Content
	r.orderMu.Unlock()
	if count != 1 || !strings.Contains(content, "child_turn_finished") || !strings.Contains(content, "cancelled") {
		t.Fatal(count, content)
	}
	var completion map[string]any
	if err := json.Unmarshal([]byte(content), &completion); err != nil {
		t.Fatal(err)
	}
	requireChildLifecycle(t, completion, "cancelled", "closed")
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
	r.Provider = &childProvider{stream: func(ctx context.Context, request llm.Request, emit func(llm.StreamEvent) error) error {
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Partial evidence"}); err != nil {
			return err
		}
		return errors.New("synthetic failure")
	}}
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"first","label":"research"}`)}})
	result := childInvocation(t, r, ids[0], `{"persistent":true,"prompt":"first","label":"research"}`)
	if result["assignment_status"] != "failed" || result["result_entry_id"] == nil || len(r.ChildViews("main")) != 0 {
		t.Fatal(result)
	}
	requireChildLifecycle(t, result, "failed", "closed")
	entry, err := r.Store.Entry(int64(result["result_entry_id"].(float64)))
	if err != nil || !strings.Contains(string(entry.Content), "Partial evidence") {
		t.Fatal(entry, err)
	}
}

func TestChildLifecycleFieldsForSuccessfulAssignments(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		for _, background := range []bool{false, true} {
			args, _ := json.Marshal(map[string]any{"prompt": "audit", "label": "audit", "persistent": persistent, "background": background})
			t.Run(string(args), func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				r.Emit = nil
				release := make(chan struct{})
				r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
					if background {
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return emit(llm.StreamEvent{Kind: "text", Text: "Audit complete"})
				}}
				_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: args}})
				result := childInvocation(t, r, ids[0], string(args))
				state := "closed"
				if persistent {
					state = "idle"
				}
				if background {
					requireChildLifecycle(t, result, "running", "running")
					close(release)
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					if _, err := r.Jobs.Wait(ctx, "main", result["job_id"].(string), nil); err != nil {
						t.Fatal(err)
					}
					r.orderMu.Lock()
					content := r.notifications[0].Content
					r.orderMu.Unlock()
					if err := json.Unmarshal([]byte(content), &result); err != nil {
						t.Fatal(err)
					}
				}
				requireChildLifecycle(t, result, "completed", state)
				if persistent {
					requireChildLifecycle(t, r.ChildViews("main")[0], "completed", "idle")
				}
			})
		}
	}
}
