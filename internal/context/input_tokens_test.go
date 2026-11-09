package context

import (
	"encoding/json"
	"testing"

	"ttc/internal/llm"
)

func localInputSnapshot() InputSnapshot {
	return InputSnapshot{
		Selection: llm.Selection{Provider: "test", Model: llm.ModelSpec{ID: "model"}},
		System:    "instructions",
		Messages:  []llm.Message{{Type: llm.TypeUserInput, Content: "hello"}},
	}
}

func TestEstimateInputCategories(t *testing.T) {
	s := localInputSnapshot()
	s.Tools = []llm.ToolDefinition{{Name: "tool", Description: "description", Parameters: json.RawMessage(`{"type":"object"}`)}}
	replay := json.RawMessage(`{"opaque":"state"}`)
	s.Messages = append(s.Messages,
		llm.Message{Type: llm.TypeAssistant, Content: "call", Calls: []llm.ToolCall{{Name: "tool", Arguments: json.RawMessage(`{}`)}}},
		llm.Message{Type: llm.TypeToolResult, Content: "result", Files: []llm.BinaryFile{{MIMEType: "image/png"}, {MIMEType: "application/pdf", Bytes: 8000}}},
		llm.Message{Type: llm.TypeAssistant, Content: "ignored", State: &llm.ReplayState{Provider: "test", Model: "model", Items: []json.RawMessage{replay}}},
		llm.Message{Type: llm.TypeRuntimeNotification, Content: "notice"})
	r := EstimateInput(s)
	content, err := s.Messages[4].ModelContent()
	if err != nil {
		t.Fatal(err)
	}
	fixed := Estimate(s.System) + 16 + Estimate("tool") + Estimate("description") + Estimate(string(s.Tools[0].Parameters))
	want := fixed + 16 + Estimate("hello") + 16 + Estimate("call") + 16 + Estimate("tool") + Estimate(`{}`) + 16 + Estimate(content) + 16 + Estimate("result") + Estimate(string(replay)) + 12096
	if r != (llm.InputEstimate{Input: want, Estimated: true}) {
		t.Fatalf("result = %+v, want input %d", r, want)
	}
	if r.Input != fixed+Tokens(s.Messages) {
		t.Fatal("local estimation disagrees with Tokens")
	}
}

func TestEstimateInputRetainedAndInvalidMetadata(t *testing.T) {
	s := localInputSnapshot()
	s.Messages[0].InputTimeMS = 1000
	s.Messages[0].RetainedAtMS = 2000
	if got := EstimateInput(s); got.Input != Estimate(s.System)+Tokens(s.Messages) {
		t.Fatalf("retained projection mismatch: %+v", got)
	}
	for name, message := range map[string]llm.Message{
		"retained without timestamp": {Type: llm.TypeUserInput, Content: "hello", RetainedAtMS: 2000},
		"invalid semantic type":      {Type: "invalid"},
		"invalid retained source":    {Type: llm.TypeUserInput, InputSource: "invalid", InputTimeMS: 1000, RetainedAtMS: 2000},
	} {
		t.Run(name, func(t *testing.T) {
			s.Messages = []llm.Message{message}
			if got := EstimateInput(s); got != (llm.InputEstimate{Input: -1, Estimated: true}) {
				t.Fatalf("invalid input accepted: %+v", got)
			}
			if got := Tokens(s.Messages); got != -1 {
				t.Fatalf("invalid input has %d tokens", got)
			}
		})
	}
}

func TestEstimateInputReplayProjectionDoesNotMutateHistory(t *testing.T) {
	s := localInputSnapshot()
	state := &llm.ReplayState{Provider: "test", Model: "model", Items: []json.RawMessage{json.RawMessage(`{"opaque":"state"}`)}}
	s.Messages = []llm.Message{{Type: llm.TypeAssistant, Content: "canonical", State: state}}
	want := Estimate(s.System) + Estimate(string(state.Items[0]))
	if got := EstimateInput(s); got.Input != want {
		t.Fatalf("native replay estimate = %+v, want %d", got, want)
	}
	s.Selection.Model.ID = "other"
	want = Estimate(s.System) + 16 + Estimate("canonical")
	if got := EstimateInput(s); got.Input != want {
		t.Fatalf("canonical projection estimate = %+v, want %d", got, want)
	}
	if s.Messages[0].State != state || string(state.Items[0]) != `{"opaque":"state"}` {
		t.Fatal("local estimation mutated canonical history")
	}
}

func TestEstimateInputRejectsOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	// Document byte reserves exercise overflow without allocating huge payloads.
	// Further additions must not wrap an invalid count back into validity.
	for _, counts := range [][]int{{maxInt}, {maxInt, maxInt, 100}} {
		s := localInputSnapshot()
		s.Messages = nil
		for _, count := range counts {
			s.Messages = append(s.Messages, llm.Message{Type: llm.TypeUserInput, Files: []llm.BinaryFile{{MIMEType: "application/pdf", Bytes: count}}})
		}
		if got := EstimateInput(s); got != (llm.InputEstimate{Input: -1, Estimated: true}) {
			t.Fatal("local overflow was not rejected", got)
		}
	}
	s := localInputSnapshot()
	s.System = ""
	s.Messages = []llm.Message{{Type: llm.TypeUserInput, Files: []llm.BinaryFile{{MIMEType: "application/pdf", Bytes: maxInt - 16}}}}
	if got := EstimateInput(s); got != (llm.InputEstimate{Input: maxInt, Estimated: true}) {
		t.Fatalf("valid boundary estimate = %+v", got)
	}
	if got := Tokens(s.Messages); got != maxInt {
		t.Fatalf("valid boundary message count = %d", got)
	}
	s.Messages = append(s.Messages, llm.Message{Type: llm.TypeAssistant, Content: "more"})
	if got := Tokens(s.Messages); got != -1 {
		t.Fatalf("overflowing message count = %d", got)
	}
}

func TestTokensPreservesAlreadySelectedReplay(t *testing.T) {
	first := json.RawMessage(`{"opaque":"first replay"}`)
	second := json.RawMessage(`{"opaque":"second replay"}`)
	messages := []llm.Message{
		{Type: llm.TypeAssistant, Content: "ignored", State: &llm.ReplayState{Provider: "one", Model: "first", Items: []json.RawMessage{first}}},
		{Type: llm.TypeAssistant, Content: "also ignored", State: &llm.ReplayState{Provider: "two", Model: "second", Items: []json.RawMessage{second}}},
	}
	want := Estimate(string(first)) + Estimate(string(second))
	if got := Tokens(messages); got != want {
		t.Fatalf("already selected replay count = %d, want %d", got, want)
	}
	if got := rawTokens(messages[:1]); got != Estimate(string(first)) {
		t.Fatalf("individual retention message count = %d, want %d", got, Estimate(string(first)))
	}
}

func TestFitsInput(t *testing.T) {
	b := llm.Budget{ContextLimit: 100, OutputAllowance: 20, EstimationMargin: 10, NextTurnInputReserve: 5}
	if !FitsInput(69, b, false) || FitsInput(70, b, false) || !FitsInput(64, b, true) || FitsInput(65, b, true) || FitsInput(-1, b, false) || FitsInput(int(^uint(0)>>1), b, false) {
		t.Fatal("incorrect strict budget boundary")
	}
	for name, budget := range map[string]llm.Budget{
		"negative context": {ContextLimit: -1},
		"negative output":  {ContextLimit: 100, OutputAllowance: -1},
		"negative margin":  {ContextLimit: 100, EstimationMargin: -1},
		"negative reserve": {ContextLimit: 100, NextTurnInputReserve: -1},
		"zero context":     {},
	} {
		t.Run(name, func(t *testing.T) {
			if FitsInput(0, budget, true) {
				t.Fatal("invalid budget accepted")
			}
		})
	}
}
