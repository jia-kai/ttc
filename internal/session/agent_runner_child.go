package session

import (
	"context"
	"errors"
	"io"
	"strings"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// assignmentAgentContext owns a private transcript and request-context cursor.
type assignmentAgentContext struct {
	runtime  *Runtime
	task     childTask
	messages []llm.Message
	cursor   contextCursor
}

func (c *assignmentAgentContext) admit(ctx context.Context, recovery *llm.Message) (agentRequest, error) {
	r := c.runtime
	compacted := false
	for {
		if err := ctx.Err(); err != nil {
			return agentRequest{}, err
		}
		r.routeMu.RLock()
		admitted, next, err := r.admitChild(ctx, c.task, c.messages, c.cursor)
		if !c.task.aside && errors.Is(err, errNeedsCompaction) {
			r.routeMu.RUnlock()
			if compacted {
				return agentRequest{}, errors.New(prompts.ChildAdmissionAfterCompaction)
			}
			c.messages, c.cursor, err = r.compactChild(ctx, c.task, c.messages, c.cursor, recovery)
			if err != nil {
				return agentRequest{}, err
			}
			compacted = true
			continue
		}
		if err != nil {
			r.routeMu.RUnlock()
			return agentRequest{}, err
		}
		c.cursor, c.messages = next, admitted.Messages
		_, err = r.Store.RecordSystemPrompt(r.Current(), c.task.turn, c.task.actor, admitted.RequestID, admitted.input.System)
		r.routeMu.RUnlock()
		if err != nil {
			return agentRequest{}, err
		}
		if err := r.deliverNoticeEntries(c.task.actor, admitted.NoticeEntries); err != nil {
			return agentRequest{}, err
		}
		return agentRequest{id: admitted.RequestID, Request: admitted.input}, nil
	}
}

func (c *assignmentAgentContext) append(messages ...llm.Message) {
	c.messages = append(c.messages, messages...)
}

func (*assignmentAgentContext) observe(int64, llm.Selection, *llm.Usage, bool) {}

type assignmentAgentPolicy struct {
	runtime *Runtime
	context *assignmentAgentContext
}

func (p *assignmentAgentPolicy) stage(reply llm.Message) {
	task, r := p.context.task, p.runtime
	if task.child == nil {
		return
	}
	r.coordinator.childMu.Lock()
	defer r.coordinator.childMu.Unlock()
	if strings.TrimSpace(reply.Content) != "" {
		task.assignment.answer, task.assignment.truncated = childAnswer(reply.Content)
		task.assignment.result = 0
		task.assignment.inheritedAnswer = false
	}
	task.assignment.pending = &reply
}

func (p *assignmentAgentPolicy) committed(reply llm.Message, entry int64) {
	task, r := p.context.task, p.runtime
	if task.child == nil {
		return
	}
	r.coordinator.childMu.Lock()
	defer r.coordinator.childMu.Unlock()
	if strings.TrimSpace(reply.Content) != "" {
		task.assignment.result = entry
	}
	task.assignment.pending = nil
}

func (p *assignmentAgentPolicy) complete(ctx context.Context, _ llm.Selection, reply llm.Message) (bool, error) {
	task, r := p.context.task, p.runtime
	notification, err := r.childImageReply(ctx, task.actor, len(reply.Calls) == 0)
	if err != nil {
		return false, err
	}
	if notification != nil {
		r.routeMu.RLock()
		id, err := r.Store.Append(r.Current(), task.turn, task.actor, "message", "user", false, *notification)
		r.routeMu.RUnlock()
		if err != nil {
			return false, err
		}
		p.context.append(*notification)
		r.emit(Event{Kind: "message", Actor: task.actor, Text: notification.Content, EntryID: id, MessageType: notification.Type})
		return false, nil
	}
	if len(reply.Calls) != 0 {
		return false, nil
	}
	if task.child != nil {
		// Final output and parent guidance arbitrate on the same admission gate.
		// Resource scopes remain open until finishChild joins them, before notice.
		r.orderMu.Lock()
		pending := len(task.assignment.queued) != 0 || task.assignment.runtimeNotices.len() != 0
		if !pending {
			task.assignment.accepting = false
		}
		r.orderMu.Unlock()
		if pending {
			return false, nil
		}
		if strings.TrimSpace(reply.Content) == "" {
			return false, errors.New(prompts.ChildCompletedWithoutText)
		}
		r.coordinator.childMu.Lock()
		task.child.messages, task.child.cursor = p.context.messages, p.context.cursor
		r.coordinator.childMu.Unlock()
	}
	return true, nil
}

// Aside output stays private until its final successful response; tool-cycle
// text and interrupted attempts must not leak into the answer buffer.
type asideAgentPolicy struct{ stdout io.Writer }

func (*asideAgentPolicy) stage(llm.Message)            {}
func (*asideAgentPolicy) committed(llm.Message, int64) {}
func (p *asideAgentPolicy) complete(_ context.Context, _ llm.Selection, reply llm.Message) (bool, error) {
	if len(reply.Calls) != 0 {
		return false, nil
	}
	_, err := io.WriteString(p.stdout, reply.Content)
	return true, err
}
