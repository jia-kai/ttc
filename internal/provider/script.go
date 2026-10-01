package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// ScriptResponse is a deterministic offline response used for local integration runs.
type ScriptResponse struct {
	Prefix string     `json:"prefix,omitempty"`
	Text   string     `json:"text"`
	Calls  []ToolCall `json:"calls,omitempty"`
}

// Script provides explicit offline test mode; it never accesses credentials or the network.
type Script struct {
	Responses []ScriptResponse
	mu        sync.Mutex
	next      int
}

// ScriptModel supplies a synthetic budget, never a claim about a real model.
func ScriptModel() ModelSpec {
	return ModelSpec{ID: "scripted", Name: "Offline scripted provider", Variants: []string{"none"}, DefaultVariant: "none", Budget: Budget{ContextLimit: 32768, MaxOutputTokens: 4096, OutputAllowance: 1024, EstimationMargin: 512, RecentTokensTarget: 4096, NextTurnInputReserve: 1024, SummaryOutputAllowance: 1024}, Revision: "test-v1"}
}

// Models returns the single offline model.
func (s *Script) Models(context.Context) ([]ModelSpec, error) { return []ModelSpec{ScriptModel()}, nil }

// Login rejects authentication in explicit offline mode.
func (s *Script) Login(context.Context, LoginUI) error {
	return errors.New("offline mode has no login")
}

// Stream emits the next fixed response; exhausted scripts fail instead of inventing output.
func (s *Script) Stream(ctx context.Context, req Request, emit func(StreamEvent) error) error {
	s.mu.Lock()
	if s.next >= len(s.Responses) {
		s.mu.Unlock()
		return errors.New("offline script exhausted")
	}
	v := s.Responses[s.next]
	s.next++
	s.mu.Unlock()
	if v.Prefix != "" {
		last := len(req.Messages) - 1
		for last >= 0 && req.Messages[last].Role == "developer" {
			last--
		}
		if last < 0 || !strings.HasPrefix(req.Messages[last].Role+": "+req.Messages[last].Content, v.Prefix) {
			return errors.New("offline script prefix mismatch: expected " + v.Prefix)
		}
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if req.NoTools && len(v.Calls) > 0 {
		return errors.New("script returned tools to a no-tools request")
	}
	if v.Text != "" {
		if e := emit(StreamEvent{Kind: "text", Text: v.Text}); e != nil {
			return e
		}
	}
	for _, c := range v.Calls {
		c := c
		if e := emit(StreamEvent{Kind: "call", Call: &c}); e != nil {
			return e
		}
	}
	return emit(StreamEvent{Kind: "completed", Usage: &Usage{InputTokens: len(req.System) / 3, OutputTokens: len(v.Text) / 3}, ResponseID: "offline"})
}
