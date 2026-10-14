package session

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/tool"
)

// Bound both accumulated payload and envelope count, even for tiny messages.
// The per-assignment token ceiling also respects reserved compaction headroom.
const maxChildMessageQueueTokens = 4096
const maxChildMessageQueueCount = 32

// addChildMessageTool sends to an existing assignment, not to the retained
// context. Only the worker consumes its queue, at settled request boundaries.
func (r *Runtime) addChildMessageTool() {
	type args struct {
		ChildID string `json:"child_id,omitempty"`
		TurnID  string `json:"child_turn_id,omitempty"`
		JobID   string `json:"job_id,omitempty"`
		Message string `json:"message"`
	}
	tool.Register(r.Tools, "child_message", prompts.ToolDescription("child_message"), map[string]any{
		"child_id": tool.Property("string"), "child_turn_id": tool.Property("string"), "job_id": tool.Property("string"), "message": tool.Property("string"),
	}, []string{"message"}, func(a args) error {
		if !utf8.ValidString(a.Message) || strings.TrimSpace(a.Message) == "" {
			return tool.Fail("invalid_arguments", prompts.ChildMessageNonempty)
		}
		if (a.JobID != "" && (a.ChildID != "" || a.TurnID != "")) || (a.JobID == "" && (a.ChildID == "" || a.TurnID == "")) {
			return tool.Fail("invalid_arguments", prompts.ChildMessageTarget)
		}
		return nil
	}, func(ctx context.Context, x tool.Execution, a args) (any, error) {
		if x.Actor != "main" {
			return nil, tool.Fail("ownership", prompts.ChildMessageOwnership)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := r.checkContext(); err != nil {
			return nil, err
		}
		r.childStartMu.Lock()
		defer r.childStartMu.Unlock()
		child := r.children[a.ChildID]
		if a.JobID != "" {
			for _, candidate := range r.children {
				if candidate.job == a.JobID {
					child = candidate
					break
				}
			}
		}
		if child == nil || child.state != "running" || child.closing || child.assignment == nil || a.TurnID != "" && child.turn != a.TurnID {
			return nil, tool.Fail("assignment_inactive", prompts.ChildMessageInactive)
		}
		r.routeMu.RLock()
		defer r.routeMu.RUnlock()
		r.orderMu.Lock()
		defer r.orderMu.Unlock()
		assignment := child.assignment
		if !assignment.accepting {
			return nil, tool.Fail("assignment_inactive", prompts.ChildMessageInactive)
		}
		remaining := assignment.queueTokenLimit - assignment.queuedTokens
		if len(assignment.queued) >= maxChildMessageQueueCount || len(a.Message) > 3*remaining {
			return nil, tool.Fail("message_queue_capacity", fmt.Sprintf(prompts.ChildMessageQueueCapacity, remaining, maxChildMessageQueueCount))
		}
		body := map[string]any{"type": "child_message", "child_id": child.id, "child_turn_id": child.turn, "job_id": child.job, "message": a.Message, "parent_call_id": x.CallID}
		content, _ := json.Marshal(body)
		// Reserve the widest possible event identity before committing evidence.
		// Escaping and routing overhead count against the same queue budget.
		tokens := contextbuild.Tokens([]llm.Message{{Role: "user", Runtime: true, Content: eventContent(string(content), math.MaxInt64)}})
		if tokens > remaining {
			return nil, tool.Fail("message_queue_capacity", fmt.Sprintf(prompts.ChildMessageQueueCapacity, remaining, maxChildMessageQueueCount))
		}
		id, err := r.Store.Append(r.Current(), "", "main", "status", "", false, body)
		if err != nil {
			return nil, err
		}
		// Parent guidance is delivered once as runtime input. Compaction may
		// summarize it; it is not independently retained as a human prompt.
		assignment.queued = append(assignment.queued, llm.Message{Role: "user", Runtime: true, Content: eventContent(string(content), id), EventSeq: id})
		assignment.queuedTokens += tokens
		return map[string]any{"state": "queued", "delivered": false, "message_event_seq": id, "child_id": child.id, "child_turn_id": child.turn, "job_id": child.job}, nil
	})
}
