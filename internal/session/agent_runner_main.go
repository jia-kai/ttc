package session

import (
	"context"
	"errors"
	"fmt"

	"ttc/internal/llm"
)

// mainAgentContext follows the durable branch, including compaction handoffs.
// Main's estimator remains session-owned and is never shared with assignments.
type mainAgentContext struct {
	runtime *Runtime
	turn    string
}

func (c *mainAgentContext) admit(ctx context.Context, recovery *llm.Message) (agentRequest, error) {
	r := c.runtime
	for {
		if event, err := r.ApplyModel(c.turn); err != nil {
			return agentRequest{}, err
		} else if event.Kind != "" {
			r.emit(event)
		}
		selection := r.CurrentSelection()
		if err := ctx.Err(); err != nil {
			return agentRequest{}, err
		}
		admitted, _, err := r.admitMain(ctx, c.turn, selection)
		if errors.Is(err, errNeedsCompaction) {
			r.emit(Event{Kind: "status", Text: "Compacting context…"})
			if _, err := r.compactContext(ctx, "", selection, recovery, false); err != nil {
				return agentRequest{}, fmt.Errorf("automatic compaction failed: %w", err)
			}
			continue
		}
		if err != nil {
			return agentRequest{}, err
		}
		for _, committed := range admitted.SteerEntries {
			message := committed.Message
			r.emit(Event{Kind: "message", Text: message.DisplayText(), EntryID: committed.EntryID, MessageType: message.Type, InputSource: message.InputSource, InputTimeMS: committed.CreatedMS})
		}
		if err := r.deliverNoticeEntries("main", admitted.NoticeEntries); err != nil {
			return agentRequest{}, err
		}
		if _, err := r.Store.RecordSystemPrompt(r.Current(), c.turn, "main", admitted.RequestID, systemTemplate); err != nil {
			return agentRequest{}, err
		}
		r.mu.Lock()
		r.mainPrefix = append([]llm.Message(nil), admitted.Messages...)
		r.prefixSelection, r.prefixTurn = selection, c.turn
		r.prefixSession, r.prefixTip = r.current, admitted.EntryTip
		r.mu.Unlock()
		return agentRequest{id: admitted.RequestID, conversation: r.Current(), selection: selection, system: systemTemplate, messages: admitted.Messages}, nil
	}
}

func (*mainAgentContext) append(...llm.Message) {} // Durable commits already extend main context.

func (c *mainAgentContext) observe(request int64, selection llm.Selection, usage *llm.Usage, success bool) {
	r := c.runtime
	if success {
		if usage.Usable() {
			if result, ok := r.estimator.ObserveUsage(request, usage.InputTokens); ok {
				r.mu.Lock()
				r.usage = usageFromEstimate(selection, result)
				r.mu.Unlock()
			}
		}
		r.mu.Lock()
		r.reported = nil
		if usage.Usable() {
			r.reported = copyReported(&ReportedUsage{Model: selection.Model.ID + " · " + selection.Variant, Tokens: *usage})
		}
		r.mu.Unlock()
	}
}

type mainAgentPolicy struct {
	runtime       *Runtime
	turn          string
	input         *llm.Message
	namingChecked bool
}

func (*mainAgentPolicy) stage(llm.Message)            {}
func (*mainAgentPolicy) committed(llm.Message, int64) {}

func (p *mainAgentPolicy) complete(_ context.Context, selection llm.Selection, reply llm.Message) (bool, error) {
	if !p.namingChecked && p.input != nil && p.runtime.AutoName {
		p.namingChecked = true
		p.runtime.startNaming(p.turn, selection, *p.input, reply)
	}
	return len(reply.Calls) == 0 && p.runtime.finishMainIfIdle(), nil
}
