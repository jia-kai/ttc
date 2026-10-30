package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

func TestSubagentReadGrantDeliveryAndLifetime(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	seedRuntime(t, r, "parent")
	id, err := r.Jobs.StartTask("main", jobs.TaskOptions{Kind: "fixture"}, func(_ context.Context, out, _ io.Writer) (jobs.TaskResult, error) {
		_, _ = io.WriteString(out, "fixture")
		return jobs.TaskResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Jobs.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		found := false
		for _, m := range req.Messages {
			if strings.Contains(m.Content, id) && strings.Contains(m.Content, "job_read_grants") && strings.Contains(m.Content, "task") {
				found = true
				if !strings.Contains(m.Content, prompts.ChildJobReadGrants) || strings.Contains(m.Content, "%!") {
					t.Errorf("bad assignment grant notice: %s", m.Content)
				}
			}
		}
		if !found {
			t.Error("assignment missing grant")
		}
		if _, err := r.Jobs.Read(ctx, req.ConversationID, id, jobs.ReadOptions{Limit: 100}); err != nil {
			t.Error(err)
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "done"})
	}}
	maxIDs := make([]string, maxJobReadGrants)
	for i := range maxIDs {
		maxIDs[i] = id
	}
	args, _ := json.Marshal(map[string]any{"persistent": true, "prompt": "task", "label": "reader", "job_read_grants": maxIDs})
	_, calls := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: args}})
	invalid, _ := json.Marshal(map[string]any{"persistent": true, "prompt": "task", "label": "reader", "job_read_grants": []string{id, "missing"}})
	if result := childInvocation(t, r, calls[0], string(invalid)); result["ok"] == true || len(r.ChildViews("main")) != 0 {
		t.Fatal("invalid mixed grant launched a child", result)
	}
	oversized := append(append([]string(nil), maxIDs...), id)
	invalid, _ = json.Marshal(map[string]any{"persistent": true, "prompt": "task", "label": "reader", "job_read_grants": oversized})
	if result := childInvocation(t, r, calls[0], string(invalid)); result["ok"] == true || !strings.Contains(fmt.Sprint(result), "at most 128 IDs") || len(r.ChildViews("main")) != 0 {
		t.Fatal("oversized grant launched a child", result)
	}
	child := childInvocation(t, r, calls[0], string(args))
	actor := child["child_id"].(string)
	if _, err := r.Jobs.Read(context.Background(), actor, id, jobs.ReadOptions{Limit: 100}); err != nil {
		t.Fatal(err)
	}
	// A retained context keeps the capability across an assignment boundary.
	args, _ = json.Marshal(map[string]any{"persistent": true, "prompt": "task", "child_id": actor})
	childInvocation(t, r, calls[0], string(args))
	if _, err := r.StopChild(context.Background(), "main", actor); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Jobs.Read(context.Background(), actor, id, jobs.ReadOptions{Limit: 100}); err == nil {
		t.Fatal("grant survived child closure")
	}
}
