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
const maxJobReadGrants = 128

func validateReadGrantCount(ids []string) error {
	if len(ids) > maxJobReadGrants {
		return tool.Fail("invalid_arguments", fmt.Sprintf(prompts.ToolJobReadGrantsLimit, maxJobReadGrants))
	}
	return nil
}

// distinctReadGrants bounds repeated work without changing the first-seen order.
func distinctReadGrants(ids []string) []string {
	if len(ids) < 2 {
		return ids
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// addChildMessageTool sends to an existing assignment, not to the retained
// context. Only the worker consumes its queue, at settled request boundaries.
func (r *Runtime) addChildMessageTool() {
	type args struct {
		ChildID       string   `json:"child_id,omitempty"`
		TurnID        string   `json:"child_turn_id,omitempty"`
		JobID         string   `json:"job_id,omitempty"`
		Message       string   `json:"message"`
		JobReadGrants []string `json:"job_read_grants,omitempty"`
	}
	tool.Register(r.Tools, "child_message", prompts.ToolDescription("child_message"), map[string]any{
		"child_id": tool.Property("string"), "child_turn_id": tool.Property("string"), "job_id": tool.Property("string"), "message": tool.Property("string"), "job_read_grants": map[string]any{"type": "array", "items": tool.Property("string"), "maxItems": maxJobReadGrants},
	}, []string{"message"}, func(a args) error {
		if err := validateReadGrantCount(a.JobReadGrants); err != nil {
			return err
		}
		if !utf8.ValidString(a.Message) || strings.TrimSpace(a.Message) == "" {
			return tool.Fail("invalid_arguments", prompts.ChildMessageNonempty)
		}
		if a.JobID == "" && (a.ChildID == "" || a.TurnID == "") {
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
		if err := r.checkContext(false); err != nil {
			return nil, err
		}
		a.JobReadGrants = distinctReadGrants(a.JobReadGrants)
		if err := r.Jobs.ValidateReadGrants(x.Actor, a.JobReadGrants); err != nil {
			return nil, tool.Fail("not_found", err.Error())
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
		if child == nil || child.state != "running" || child.closing || child.assignment == nil || a.TurnID != "" && child.turn != a.TurnID || a.ChildID != "" && child.id != a.ChildID || a.JobID != "" && child.job != a.JobID {
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
		if len(a.JobReadGrants) != 0 {
			body["job_read_grants"] = a.JobReadGrants
			body["job_read_grant_notice"] = prompts.ChildJobReadGrants
		}
		content, _ := json.Marshal(body)
		// Reserve the widest possible event identity before committing evidence.
		// Escaping and routing overhead count against the same queue budget.
		tokens := contextbuild.Tokens([]llm.Message{{Type: llm.TypeChildGuidance, Content: eventContent(string(content), math.MaxInt64, 0)}})
		if tokens > remaining {
			return nil, tool.Fail("message_queue_capacity", fmt.Sprintf(prompts.ChildMessageQueueCapacity, remaining, maxChildMessageQueueCount))
		}
		id, err := r.Store.Append(r.Current(), "", "main", "status", "", false, body)
		if err != nil {
			return nil, err
		}
		// Parent guidance is delivered once as runtime input. Compaction may
		// summarize it; it is not independently retained as a human prompt.
		assignment.queued = append(assignment.queued, llm.Message{Type: llm.TypeChildGuidance, Content: eventContent(string(content), id, 0), EventSeq: id})
		if len(a.JobReadGrants) != 0 {
			if assignment.queuedReadGrants == nil {
				assignment.queuedReadGrants = map[int64][]string{}
			}
			assignment.queuedReadGrants[id] = a.JobReadGrants
		}
		assignment.queuedTokens += tokens
		r.signalActorLocked(child.id)
		return map[string]any{"state": "queued", "delivered": false, "message_event_seq": id, "child_id": child.id, "child_turn_id": child.turn, "job_id": child.job}, nil
	})
}
