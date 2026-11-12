package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"ttc/internal/prompts"
	"ttc/internal/tool"
)

const maxParentNotificationBytes = 2048
const maxParentNotificationsPerAssignment = 32

// addNotifyParentTool installs this capability only in a coding child's registry.
// A successful send is durable input for main, not assignment completion.
func (r *Runtime) addNotifyParentTool(registry *tool.Registry, actor string) {
	type args struct {
		Message string `json:"message"`
	}
	tool.Register(registry, "notify_parent", prompts.ToolDescription("notify_parent"), map[string]any{
		"message": tool.Property("string"),
	}, []string{"message"}, func(a args) error {
		if !utf8.ValidString(a.Message) || strings.TrimSpace(a.Message) == "" {
			return tool.Fail("invalid_arguments", prompts.NotifyParentNonempty)
		}
		if len(a.Message) > maxParentNotificationBytes {
			return tool.Fail("invalid_arguments", fmt.Sprintf(prompts.NotifyParentTooLarge, maxParentNotificationBytes))
		}
		return nil
	}, func(ctx context.Context, x tool.Execution, a args) (any, error) {
		if x.Actor != actor || actor == "main" {
			return nil, tool.Fail("ownership", prompts.NotifyParentOwnership)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := r.checkContext(false); err != nil {
			return nil, err
		}
		r.coordinator.childMu.Lock()
		defer r.coordinator.childMu.Unlock()
		child := r.coordinator.children[actor]
		if child == nil || child.state != "running" || child.closing || child.assignment == nil || child.job == "" {
			return nil, tool.Fail("assignment_inactive", prompts.NotifyParentInactive)
		}
		r.routeMu.RLock()
		defer r.routeMu.RUnlock()
		r.orderMu.Lock()
		defer r.orderMu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		assignment := child.assignment
		if !assignment.accepting || r.actorAssignments[actor] != assignment {
			return nil, tool.Fail("assignment_inactive", prompts.NotifyParentInactive)
		}
		if assignment.parentNotifications >= maxParentNotificationsPerAssignment {
			return nil, tool.Fail("notification_capacity", fmt.Sprintf(prompts.NotifyParentCapacity, maxParentNotificationsPerAssignment))
		}
		body := map[string]any{"type": "child_notification", "child_id": child.id, "child_turn_id": child.turn, "job_id": child.job, "message": a.Message}
		// The destination owns the source: main's selected history and recovery
		// must retain this input even though its body identifies the sending child.
		id, err := r.Store.Append(r.Current(), "", "main", "status", "", false, map[string]any{"type": "runtime_event", "body": body})
		if err != nil {
			return nil, err
		}
		assignment.parentNotifications++
		if err := r.enqueueMainNotificationLocked(id, ""); err != nil {
			r.orderError = errors.Join(r.orderError, err)
			return nil, err
		}
		return map[string]any{"state": "queued", "delivered": false, "message_event_seq": id, "child_id": child.id, "child_turn_id": child.turn, "job_id": child.job}, nil
	})
}
