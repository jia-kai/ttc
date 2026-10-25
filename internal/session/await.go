package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/tool"
)

// signalActorLocked is paired with queue admission under orderMu. The channel
// is only a hint: pending input is checked under that same lock before waiting.
func (r *Runtime) signalActorLocked(actor string) {
	ch := r.actorSignalLocked(actor)
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (r *Runtime) actorSignalLocked(actor string) chan struct{} {
	ch := r.actorSignals[actor]
	if ch == nil {
		ch = make(chan struct{}, 1)
		r.actorSignals[actor] = ch
	}
	return ch
}

// queueActorNotificationLocked routes only to the active owning assignment.
// Assignment cleanup joins publishers before removing this routing entry and
// opening its successor, so an old callback cannot target a new assignment.
func (r *Runtime) queueActorNotificationLocked(actor, content string) error {
	if strings.HasPrefix(actor, "main/btw_") {
		actor = "main"
	}
	assignment := r.actorAssignments[actor]
	if actor != "main" && (assignment == nil || !assignment.accepting) {
		return nil
	}
	id, err := r.Store.Append(r.Current(), "", actor, "status", "", false, map[string]any{"type": "runtime_event", "body": json.RawMessage(content)})
	if err != nil {
		r.orderError = errors.Join(r.orderError, err)
		return err
	}
	m := llm.Message{Type: llm.TypeRuntimeNotification, Content: eventContent(content, id, 0), EventSeq: id}
	// Coalesce repeated timer firings just as the main notification queue does.
	var incoming struct {
		ID string `json:"wakeup_id"`
	}
	_ = json.Unmarshal([]byte(content), &incoming)
	if assignment != nil {
		assignment.queueRuntimeNotice(id, incoming.ID)
		r.signalActorLocked(actor)
		return nil
	}
	if incoming.ID != "" {
		for i, old := range r.notifications {
			var prior struct {
				ID string `json:"wakeup_id"`
			}
			if json.Unmarshal([]byte(old.Content), &prior) == nil && prior.ID == incoming.ID {
				r.notifications = append(r.notifications[:i], r.notifications[i+1:]...)
				break
			}
		}
	}
	r.notifications = append(r.notifications, m)
	r.signalActorLocked(actor)
	return nil
}

// Awaiting reports transient inference suspension, not assignment completion.
func (r *Runtime) Awaiting(actor string) bool {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	return r.awaiting[actor]
}

type awaitRequest struct {
	duration time.Duration
	timed    bool // Distinguishes an explicit zero from an indefinite wait.
}

// settleAwait parks only at a balanced tool-result boundary. The await tool
// itself has already completed and all sibling foreground calls are settled.
func (r *Runtime) settleAwait(ctx context.Context, actor string) error {
	r.orderMu.Lock()
	request, ok := r.awaitRequests[actor]
	delete(r.awaitRequests, actor)
	r.orderMu.Unlock()
	if !ok {
		return nil
	}
	var deadline time.Time
	if request.timed {
		deadline = time.Now().Add(request.duration)
	}
	reason, err := r.awaitActor(ctx, actor, deadline)
	if err == nil && reason == "deadline" {
		r.orderMu.Lock()
		err = r.queueActorNotificationLocked(actor, `{"type":"await_deadline"}`)
		r.orderMu.Unlock()
	}
	return err
}

func (r *Runtime) awaitActor(ctx context.Context, actor string, deadline time.Time) (string, error) {
	r.orderMu.Lock()
	if actor != "main" && r.actorAssignments[actor] == nil {
		r.orderMu.Unlock()
		return "", tool.Fail("assignment_inactive", prompts.AwaitActorInactive)
	}
	ch := r.actorSignalLocked(actor)
	// Clear stale hints; only unadmitted input can resume a new wait.
	select {
	case <-ch:
	default:
	}
	pending := len(r.notifications) != 0 || len(r.steers) != 0
	if actor != "main" {
		assignment := r.actorAssignments[actor]
		pending = len(assignment.queued) != 0 || len(assignment.runtimeNotices) != 0
	}
	if pending {
		r.orderMu.Unlock()
		return "pending_input", nil
	}
	r.awaiting[actor] = true
	r.orderMu.Unlock()
	r.emit(Event{Kind: "awaiting", Actor: actor, Text: "Awaiting events"})
	defer func() {
		r.orderMu.Lock()
		delete(r.awaiting, actor)
		r.orderMu.Unlock()
		r.emit(Event{Kind: "resumed", Actor: actor, Text: "Resuming"})
	}()
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-ch:
		return "event", nil
	case <-timeout:
		return "deadline", nil
	}
}

func (r *Runtime) addAwaitTool() {
	type args struct {
		Deadline *int `json:"deadline_seconds,omitempty"`
	}
	tool.Register(r.Tools, "await", prompts.ToolDescription("await"), map[string]any{"deadline_seconds": tool.IntegerRange(0, maxWakeupSeconds)}, nil, func(a args) error {
		if a.Deadline != nil && (*a.Deadline < 0 || *a.Deadline > maxWakeupSeconds) {
			return tool.Fail("invalid_arguments", prompts.AwaitInvalidDeadline)
		}
		return nil
	}, func(ctx context.Context, x tool.Execution, a args) (any, error) {
		r.orderMu.Lock()
		defer r.orderMu.Unlock()
		if x.Actor != "main" && r.actorAssignments[x.Actor] == nil {
			return nil, tool.Fail("assignment_inactive", prompts.AwaitActorInactive)
		}
		if _, pending := r.awaitRequests[x.Actor]; pending {
			return nil, tool.Fail("await_pending", prompts.AwaitAlreadyRequested)
		}
		request := awaitRequest{}
		if a.Deadline != nil {
			request.duration = time.Duration(*a.Deadline) * time.Second
			request.timed = true
		}
		r.awaitRequests[x.Actor] = request
		return map[string]any{"state": "awaiting"}, nil
	})
}
