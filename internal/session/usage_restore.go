package session

import (
	"fmt"

	"ttc/internal/llm"
)

// preparedAccounting stages conversation-derived presentation independently of
// active runtime state. Restored provider evidence is content-keyed and may be
// retained even if a later activation step fails.
type preparedAccounting struct {
	usage    ContextUsage
	reported *ReportedUsage
}

func (r *Runtime) prepareAccounting(session string, selection llm.Selection) (preparedAccounting, error) {
	state, err := r.Store.ReadContextState(r.ctx, session)
	if err != nil {
		return preparedAccounting{}, fmt.Errorf("read saved context: %w", err)
	}
	saved := state.Usage
	if anchor := saved.Anchor; anchor != nil && anchor.Checkpoint != nil {
		if err := r.Provider.RestoreEstimate(*anchor.Checkpoint, anchor.Tokens); err != nil {
			return preparedAccounting{}, fmt.Errorf("restore token checkpoint: %w", err)
		}
	}
	// Reset rebuilds configured defaults rather than active-registry extensions.
	result, err := r.estimateInput(r.ctx, llm.Request{
		ConversationID: session, Selection: selection, System: systemTemplate,
		Tools: r.defaultTools, Messages: state.Messages,
		OutputTokens: selection.Model.Budget.OutputAllowance,
	})
	if err != nil {
		return preparedAccounting{}, fmt.Errorf("estimate saved context: %w", err)
	}
	accounting := preparedAccounting{usage: usageFromEstimate(selection, result)}
	if last := saved.Reported; last != nil {
		accounting.reported = copyReported(&ReportedUsage{Model: last.Selection.Model.ID + " · " + last.Selection.Variant, Tokens: last.Tokens})
	}
	return accounting, nil
}

// replaceAccounting installs prepared conversation-derived state, never run totals.
func (r *Runtime) replaceAccounting(accounting preparedAccounting) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usage = accounting.usage
	r.reported = accounting.reported
}
