package session

import (
	"context"
	"encoding/json"
	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

// ContextUsage describes the latest parent request's frozen model and reserves.
// Admission and compaction handoffs refresh parent context fields; explicit
// session changes clear those fields but preserve the independent run totals.
type ContextUsage struct {
	Model                  string
	Limit, Input, Reserved int
	Estimated              bool           // Input includes local estimation rather than an exact matching provider report.
	Reported               *ReportedUsage // Latest successful parent response, independent of the current estimate.
	Totals                 UsageTotals    // In-memory endpoint usage since process start or /new, including all agents.
}

// UsageTotals accumulates each inference response once, including children,
// asides, naming and compaction. Totals survive compaction and session changes;
// only /new or constructing a new runtime clears them. History never restores
// them. Missing usage cannot be inferred. Optional counters are unavailable if
// any reported response omitted that counter.
// Cached reads and cache writes are input subsets; reasoning is an output subset.
type UsageTotals struct {
	Requests, ReportedRequests int
	Tokens                     llm.Usage
}

// ReportedUsage contains endpoint counters for one completed parent request.
// Cached input and reasoning output are subsets, not extra tokens. Missing
// optional counters remain unavailable rather than being presented as zero.
type ReportedUsage struct {
	Model  string
	Tokens llm.Usage
}

func copyReported(u *ReportedUsage) *ReportedUsage {
	if u == nil {
		return nil
	}
	v := *u
	v.Tokens = copyUsage(u.Tokens)
	return &v
}

func copyUsage(u llm.Usage) llm.Usage {
	v := u
	if p := u.CachedInputTokens; p != nil {
		n := *p
		v.CachedInputTokens = &n
	}
	if p := u.ReasoningOutputTokens; p != nil {
		n := *p
		v.ReasoningOutputTokens = &n
	}
	if p := u.CacheWriteTokens; p != nil {
		n := *p
		v.CacheWriteTokens = &n
	}
	return v
}

func (r *Runtime) recordUsage(u *llm.Usage) {
	r.mu.Lock()
	r.totals.Requests++
	if u != nil {
		t := &r.totals
		if t.ReportedRequests == 0 {
			t.Tokens = copyUsage(*u)
		} else {
			t.Tokens.InputTokens += u.InputTokens
			t.Tokens.OutputTokens += u.OutputTokens
			add := func(total **int, value *int) {
				if *total == nil || value == nil {
					*total = nil
				} else {
					**total += *value
				}
			}
			add(&t.Tokens.CachedInputTokens, u.CachedInputTokens)
			add(&t.Tokens.CacheWriteTokens, u.CacheWriteTokens)
			add(&t.Tokens.ReasoningOutputTokens, u.ReasoningOutputTokens)
		}
		t.ReportedRequests++
	}
	r.mu.Unlock()
	r.emit(Event{Kind: "usage"})
}

func estimateUsage(selection llm.Selection, system string, defs []llm.ToolDefinition, messages []llm.Message) ContextUsage {
	return usageFromEstimate(selection, contextbuild.EstimateInput(contextbuild.InputSnapshot{Selection: selection, System: system, Tools: defs, Messages: messages}))
}

func usageFromEstimate(selection llm.Selection, result contextbuild.Result) ContextUsage {
	b := selection.Model.Budget
	return ContextUsage{Model: selection.Model.ID + " · " + selection.Variant, Limit: b.ContextLimit, Input: result.Input, Estimated: result.Estimated, Reserved: b.OutputAllowance + b.EstimationMargin + b.SummaryOutputAllowance + compactionInstructionTokens()}
}

// contextMessages copies the selected provider projection and annotates replay
// occupancy without changing durable history or the payload sent to the model.
func (r *Runtime) contextMessages(selection llm.Selection, messages []llm.Message) []llm.Message {
	return contextbuild.MessagesForEstimation(selection, messages, r.Provider)
}

// UsageSnapshot returns an immutable estimate without rebuilding history on UI ticks.
func (r *Runtime) UsageSnapshot() ContextUsage {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.usage
	u.Reported = copyReported(r.reported)
	u.Totals = r.totals
	u.Totals.Tokens = copyUsage(r.totals.Tokens)
	return u
}

// TimerView contains only live timer metadata; no callbacks or retained output.
type TimerView struct {
	ID     string `json:"wakeup_id"`
	Name   string `json:"name"`
	NextAt string `json:"next_at"`
	Repeat int    `json:"repeat_seconds,omitempty"`
}

// LiveTimers returns scheduled timers only; replay never recreates timers.
func (r *Runtime) LiveTimers() []TimerView {
	r.mu.Lock()
	w := r.timers
	r.mu.Unlock()
	var out []TimerView
	for _, v := range w.list() {
		if v.Status == "scheduled" {
			out = append(out, TimerView{v.ID, v.Name, v.NextAt, v.Repeat})
		}
	}
	return out
}

// ImageForEntry reads a durable image_show result without restoring interactions.
func (r *Runtime) ImageForEntry(id int64) (*ImageSnapshot, error) {
	outcome, err := r.Store.ToolOutcomeForEntry(context.Background(), id)
	if err != nil {
		return nil, err
	}
	if outcome.Name != "image_show" {
		return nil, nil
	}
	if outcome.Result == nil {
		r.images.mu.Lock()
		p, ok := r.images.pending[outcome.CallID]
		r.images.mu.Unlock()
		if ok {
			return &p.view, nil
		}
		return nil, nil
	}
	var v ImageSnapshot
	if err = json.Unmarshal(outcome.Result, &v); err != nil {
		return nil, err
	}
	if v.Snapshot == "" {
		return nil, nil
	}
	return &v, nil
}
