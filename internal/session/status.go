package session

import (
	"database/sql"
	"encoding/json"
	contextbuild "scicode/internal/context"
	"scicode/internal/provider"
)

// TokenPart is one disjoint estimate in tokens, never endpoint-reported usage.
type TokenPart struct {
	Name   string
	Tokens int
}

// ContextUsage describes the latest parent request's frozen model and reserves.
// Admission and compaction handoffs refresh it; explicit session changes clear it.
type ContextUsage struct {
	Model                  string
	RequestID              int64 // Identity of the estimated parent request; zero before it starts.
	Limit, Input, Reserved int
	Parts                  []TokenPart
	Reported               *ReportedUsage // Latest successful parent response, independent of the current estimate.
	Totals                 UsageTotals    // Cumulative endpoint usage since this session was activated, including all agents.
}

// UsageTotals accumulates each inference response once, including children,
// asides, naming and compaction. Missing usage cannot be inferred. Optional
// counters are unavailable if any reported response omitted that counter.
// Cached reads and cache writes are input subsets; reasoning is an output subset.
type UsageTotals struct {
	Requests, ReportedRequests int
	Tokens                     provider.Usage
}

// ReportedUsage contains endpoint counters for one completed parent request.
// Cached input and reasoning output are subsets, not extra tokens. Missing
// optional counters remain unavailable rather than being presented as zero.
type ReportedUsage struct {
	Model     string
	RequestID int64 // Producing request; permits matching exact input to its estimate.
	Tokens    provider.Usage
}

func copyReported(u *ReportedUsage) *ReportedUsage {
	if u == nil {
		return nil
	}
	v := *u
	v.Tokens = copyUsage(u.Tokens)
	return &v
}

func copyUsage(u provider.Usage) provider.Usage {
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

func (r *Runtime) recordUsage(u *provider.Usage) {
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

func estimateUsage(selection provider.Selection, system string, defs []provider.ToolDefinition, messages []provider.Message) ContextUsage {
	counts := make([]int, 6)
	counts[0] = contextbuild.Estimate(system)
	for _, d := range defs {
		counts[1] += 16 + contextbuild.Estimate(d.Description) + contextbuild.Estimate(string(d.Parameters))
	}
	for _, m := range messages {
		if m.State != nil {
			counts[4] += provider.ReplayTokens(m.State)
			continue
		}
		i := 2
		if m.Role == "tool" {
			i = 3
		}
		counts[i] += 16 + contextbuild.Estimate(m.Content)
		for _, c := range m.Calls {
			counts[i] += 16 + contextbuild.Estimate(c.Name) + contextbuild.Estimate(string(c.Arguments))
		}
		counts[5] += 4096 * len(m.Images)
	}
	b := selection.Model.Budget
	u := ContextUsage{Model: selection.Model.ID + " · " + selection.Variant, Limit: b.ContextLimit, Reserved: b.OutputAllowance + b.EstimationMargin}
	for i, name := range []string{"Instructions", "Tool definitions", "Conversation", "Tool results", "Provider state", "Images"} {
		u.Parts = append(u.Parts, TokenPart{name, counts[i]})
		u.Input += counts[i]
	}
	u.Parts = append(u.Parts, TokenPart{"Output reserve", b.OutputAllowance}, TokenPart{"Safety margin", b.EstimationMargin})
	return u
}

// contextMessages copies the selected provider projection and annotates replay
// occupancy without changing durable history or the payload sent to the model.
func (r *Runtime) contextMessages(selection provider.Selection, messages []provider.Message) []provider.Message {
	out := provider.ContextFor(selection, messages)
	for i, m := range out {
		if m.State != nil {
			state := *m.State
			tokens := r.Provider.EstimateReplay(m)
			state.EstimatedTokens = &tokens
			out[i].State = &state
		}
	}
	return out
}

// UsageSnapshot returns an immutable estimate without rebuilding history on UI ticks.
func (r *Runtime) UsageSnapshot() ContextUsage {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.usage
	u.Parts = append([]TokenPart(nil), u.Parts...)
	u.Reported = copyReported(r.reported)
	u.Totals = r.totals
	u.Totals.Tokens = copyUsage(r.totals.Tokens)
	return u
}

// TimerView contains only live timer metadata; no callbacks or retained output.
type TimerView struct {
	ID, Name, NextAt string
	Repeat           int
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
	var callID, name string
	var result sql.NullString
	err := r.Store.DB.QueryRow("SELECT id,name,result_json FROM tool_calls WHERE id=coalesce((SELECT call_id FROM tool_records WHERE entry_id=?),(SELECT json_extract(content_json,'$.call_id') FROM entries WHERE id=? AND kind='tool_call'))", id, id).Scan(&callID, &name, &result)
	if err != nil {
		return nil, err
	}
	if name != "image_show" {
		return nil, nil
	}
	if !result.Valid {
		r.images.mu.Lock()
		p, ok := r.images.pending[callID]
		r.images.mu.Unlock()
		if ok {
			return &p.view, nil
		}
		return nil, nil
	}
	var v ImageSnapshot
	if err = json.Unmarshal([]byte(result.String), &v); err != nil {
		return nil, err
	}
	if v.Snapshot == "" {
		return nil, nil
	}
	return &v, nil
}
