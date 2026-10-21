package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestChildAwaitReceivesOnlyItsTimerAndMarksDelivery(t *testing.T) {
	for _, delay := range []int{0, 1} {
		t.Run(map[int]string{0: "pending_before_wait", 1: "fires_while_waiting"}[delay], func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			step := 0
			r.Provider = &childProvider{stream: func(ctx context.Context, request llm.Request, emit func(llm.StreamEvent) error) error {
				step++
				if step == 1 {
					args, _ := json.Marshal(map[string]any{"name": "owned-timer", "message": "timer evidence", "delay_seconds": delay})
					if err := emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "timer", Name: "wakeup_schedule", Arguments: args}}); err != nil {
						return err
					}
					return emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "park", Name: "await", Arguments: []byte(`{"deadline_seconds":10}`)}})
				}
				found := false
				for _, message := range request.Messages {
					found = found || message.Type == llm.TypeRuntimeNotification && strings.Contains(message.Content, "timer evidence")
				}
				if !found {
					t.Error("child did not receive its timer notification")
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Timer received"})
			}}
			_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":false,"prompt":"timer","label":"timer child"}`)}})
			result := childInvocation(t, r, ids[0], `{"persistent":false,"prompt":"timer","label":"timer child"}`)
			if result["ok"] != true || result["assignment_status"] != "completed" {
				t.Fatal(result)
			}
			timers := r.timers.list()
			if len(timers) != 1 || timers[0].Last != "delivered" {
				t.Fatal("timer firing was not marked admitted", timers)
			}
			r.orderMu.Lock()
			defer r.orderMu.Unlock()
			for _, message := range r.notifications {
				if strings.Contains(message.Content, "timer evidence") {
					t.Fatal("child timer leaked to parent", message)
				}
			}
		})
	}
}
