package history

import (
	"errors"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// RequestCompletion records a terminal request outcome and its attempts in exact
// chronological order. Attempts are immutable audit evidence, not a usage cache.
// Only a completed request's last completed attempt supplies measured usage;
// absent usage on that attempt stays absent, even if an earlier attempt had it.
// Negative input/output counts are retained in audit diagnostics but excluded
// from normalized measurements without failing the request completion.
type RequestCompletion struct {
	Status   string           `json:"status"`
	Attempts []RequestAttempt `json:"attempts"`
}

// RequestAttempt records one provider attempt. Usage reports endpoint token
// counts (not estimates); nil means unavailable, and failed attempts never supply
// a measurement. DurationMS is elapsed wall time in milliseconds; zero means
// unreported. Attempt is the provider's one-based attempt number, or zero when
// unreported. Optional diagnostics do not affect usage selection.
type RequestAttempt struct {
	Status      string     `json:"status,omitempty"`
	Usage       *llm.Usage `json:"usage,omitempty"`
	DurationMS  int64      `json:"duration_ms,omitempty"`
	ResponseID  string     `json:"response_id,omitempty"`
	ServiceTier string     `json:"service_tier,omitempty"`
	Error       string     `json:"error,omitempty"`
	Attempt     int        `json:"attempt,omitempty"`
}

func terminalRequestStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "interrupted"
}

func (c RequestCompletion) validate() error {
	if !terminalRequestStatus(c.Status) {
		return errors.New(prompts.HistoryRequestCompletionStatus)
	}
	for _, attempt := range c.Attempts {
		if attempt.Status != "" && !terminalRequestStatus(attempt.Status) {
			return errors.New(prompts.HistoryRequestCompletionStatus)
		}
		if attempt.DurationMS < 0 || attempt.Attempt < 0 {
			return errors.New(prompts.HistoryRequestAttemptMetadata)
		}
	}
	return nil
}

func (c RequestCompletion) usage() *llm.Usage {
	if c.Status == "completed" {
		for i := len(c.Attempts) - 1; i >= 0; i-- {
			if c.Attempts[i].Status == "completed" {
				usage := c.Attempts[i].Usage
				if !usage.Usable() {
					return nil
				}
				return usage
			}
		}
	}
	return nil
}
