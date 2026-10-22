package session

import (
	"fmt"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

// preparedAccounting owns a fresh estimator: preparation must never change the
// active runtime's calibration, even when a later storage operation fails.
type preparedAccounting struct {
	estimator *contextbuild.Estimator
	usage     ContextUsage
	reported  *ReportedUsage
}

func (r *Runtime) prepareAccounting(session string, selection llm.Selection) (preparedAccounting, error) {
	saved, err := r.Store.MainUsage(r.ctx, session)
	if err != nil {
		return preparedAccounting{}, fmt.Errorf("read saved token usage: %w", err)
	}
	messages, err := r.Store.Messages(session)
	if err != nil {
		return preparedAccounting{}, fmt.Errorf("read saved context: %w", err)
	}
	// Reset rebuilds the same configured defaults. Their private schema snapshot
	// stays independent of active-registry extensions, replacements and mutation.
	definitions := r.defaultTools
	accounting := preparedAccounting{estimator: &contextbuild.Estimator{}}
	if anchor := saved.Anchor; anchor != nil {
		if anchor.Checkpoint != nil {
			if err := accounting.estimator.Restore(*anchor.Checkpoint, anchor.Tokens.InputTokens, session); err != nil {
				return preparedAccounting{}, fmt.Errorf("restore token checkpoint: %w", err)
			}
		} else {
			// Legacy requests use current schemas only for approximate calibration.
			accounting.estimator.RestoreApproximate(contextbuild.InputSnapshot{
				Selection: anchor.Selection, System: anchor.System, Tools: definitions,
				Messages: anchor.Messages, Lineage: session, TokenEstimator: r.Provider,
			}, anchor.Tokens.InputTokens)
		}
	}
	result := accounting.estimator.Estimate(contextbuild.InputSnapshot{
		Selection: selection, System: systemTemplate, Tools: definitions,
		Messages: messages, Lineage: session, TokenEstimator: r.Provider,
	})
	accounting.usage = usageFromEstimate(selection, result)
	if last := saved.Reported; last != nil {
		accounting.reported = copyReported(&ReportedUsage{Model: last.Selection.Model.ID + " · " + last.Selection.Variant, Tokens: last.Tokens})
	}
	return accounting, nil
}

// replaceAccounting installs prepared conversation-derived state, never run totals.
func (r *Runtime) replaceAccounting(accounting preparedAccounting) {
	if accounting.estimator == nil {
		accounting.estimator = &contextbuild.Estimator{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.estimator = accounting.estimator
	r.usage = accounting.usage
	r.reported = accounting.reported
}
