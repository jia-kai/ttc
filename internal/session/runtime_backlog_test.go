package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"ttc/internal/historytest"
	"ttc/internal/llm"
)

func TestChildRuntimeBacklogBatchesAndAcknowledgesEverySource(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	step := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, request llm.Request, emit func(llm.StreamEvent) error) error {
		step++
		if step == 1 {
			r.orderMu.Lock()
			for i := range 40 {
				event := map[string]any{"type": "job_exit", "job_id": fmt.Sprintf("job_%d", i), "status": "completed", "stdout": "small"}
				if i == 0 {
					event["stdout"] = strings.Repeat("\x00", 8192)
					event["kind"] = "watch"
					event["exit_code"] = 7
					event["signal"] = "SIGTERM"
				}
				body, _ := json.Marshal(event)
				if err := r.queueActorNotificationLocked(request.ConversationID, string(body)); err != nil {
					r.orderMu.Unlock()
					return err
				}
			}
			assignment := r.actorAssignments[request.ConversationID]
			if len(assignment.queued) != 0 || assignment.queuedTokens != 0 {
				t.Error("runtime activity consumed parent guidance budget")
			}
			r.orderMu.Unlock()
			return emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "await", Name: "await", Arguments: []byte(`{}`)}})
		}
		// The oversized first source must become a compact, individually acknowledged
		// reference, not an impossible admission/compaction loop.
		if step == 2 {
			found := false
			for _, message := range request.Messages {
				if message.Type != llm.TypeRuntimeNotification || !strings.Contains(message.Content, `"job_id":"job_0"`) {
					continue
				}
				found = strings.Contains(message.Content, `"output_omitted":true`)
				var outcome struct {
					Kind     string `json:"kind"`
					ExitCode *int   `json:"exit_code"`
					Signal   string `json:"signal"`
				}
				if err := json.Unmarshal([]byte(message.Content), &outcome); err != nil || outcome.Kind != "watch" || outcome.ExitCode == nil || *outcome.ExitCode != 7 || outcome.Signal != "SIGTERM" {
					t.Error("compact reference lost workload outcome", message.Content, err)
				}
			}
			if !found {
				t.Error("oversized source did not become a compact reference")
			}
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Batch received"})
	}}
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":false,"prompt":"drain events","label":"backlog child"}`)}})
	result := childInvocation(t, r, ids[0], `{"persistent":false,"prompt":"drain events","label":"backlog child"}`)
	if result["ok"] != true || result["assignment_status"] != "completed" {
		t.Fatal(result)
	}
	if step < 3 {
		t.Fatal("backlog was not admitted in bounded batches", step)
	}
	var sources, acknowledged int
	err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*),count(delivered_request_id) FROM entries WHERE actor_id=? AND json_extract(content_json,'$.type')='runtime_event'`, result["child_id"]).Scan(&sources, &acknowledged)
	if err != nil || sources != 40 || acknowledged != sources {
		t.Fatal("source acknowledgments were lost", sources, acknowledged, err)
	}
}
