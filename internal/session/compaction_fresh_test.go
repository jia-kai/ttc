package session

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

func TestPrepareCompactionSeparatesHighContextPrefixFromFreshBatch(t *testing.T) {
	for _, test := range []struct {
		name                             string
		capacity, oldTokens, freshTokens int
	}{
		{"scaled", 8000, 5800, 2600},
		{"400k context", 400000, 340000, 70000},
	} {
		t.Run(test.name, func(t *testing.T) {
			selection := llm.Selection{Model: llm.ScriptModel()}
			selection.Model.Budget.ContextLimit = test.capacity
			selection.Model.Budget.SummaryOutputAllowance = 1000
			selection.Model.Budget.EstimationMargin = 100
			selection.Model.Budget.RecentTokensMin = 0
			selection.Model.Budget.RecentTokensMax = 700
			messages := []llm.Message{
				{Type: llm.TypeUserInput, Content: "Research", InputSource: "task", InputTimeMS: time.Now().Add(-time.Minute).UnixMilli()},
				{Type: llm.TypeAssistant, Content: strings.Repeat("older fact ", test.oldTokens*3/11)},
				{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "a"}, {ID: "b"}}, State: &llm.ReplayState{Provider: "script", Model: "fixture", Version: 1}},
				{Type: llm.TypeToolResult, CallID: "b", Content: "fresh b " + strings.Repeat("b", test.freshTokens*3/2)},
				{Type: llm.TypeToolResult, CallID: "a", Content: "fresh a " + strings.Repeat("a", test.freshTokens*3/2)},
				{Type: llm.TypeUserInput, InputSource: "steer", Content: "Continue", InputTimeMS: time.Now().Add(-time.Second).UnixMilli()},
			}
			original := append([]llm.Message(nil), messages...)
			prepared, err := prepareCompaction(messages, selection)
			if err != nil || prepared.retention.Start != 2 || len(prepared.inputs) != 1 || prepared.inputs[0].InputSource != "task" {
				t.Fatal("incorrect protected cycle or input source", prepared.retention, err)
			}
			if strings.Contains(prepared.transcript, "fresh ") || !strings.Contains(prepared.transcript, "older fact") || prepared.messages[2].State != nil || !reflect.DeepEqual(original, messages) {
				t.Fatal("summary consumed fresh output or preparation mutated admitted input")
			}
			if !reflect.DeepEqual(prepared.messages[3:], messages[3:]) {
				t.Fatal("fresh result bodies or input source changed")
			}
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Research")
			requests := 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				requests++
				if strings.Contains(req.Messages[0].Content, "fresh ") {
					t.Fatal("fresh batch reached summarizer")
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Older facts summarized."})
			}}
			// The old behavior included the fresh batch and overflowed before inference.
			full, err := summaryTranscript(canonicalCompaction(messages))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.summarize(context.Background(), "main", "", selection, full, ""); err == nil || !strings.Contains(err.Error(), "estimated input") || requests != 0 {
				t.Fatal("fixture did not reproduce pre-summary capacity failure", err, requests)
			}
			if _, err := r.summarize(context.Background(), "main", "", selection, prepared.transcript, ""); err != nil || requests != 1 {
				t.Fatal("older prefix did not fit separately from fresh batch", err, requests)
			}
		})
	}
}

func TestCompactionWithoutOlderWorkPreservesFreshResults(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedRuntime(t, r, "Research")
	messages := []llm.Message{
		{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "fresh"}}},
		{Type: llm.TypeToolResult, CallID: "fresh", Content: strings.Repeat("fresh result ", 300)},
	}
	for _, message := range messages {
		if _, err := r.Store.Append(r.Current(), "", "main", "message", message.Role(), true, message); err != nil {
			t.Fatal(err)
		}
	}
	before := r.Current()
	original, err := r.Store.Messages(before)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	r.Provider = &childProvider{stream: func(context.Context, llm.Request, func(llm.StreamEvent) error) error { requests++; return nil }}
	if _, err := r.compactContext(context.Background(), "", r.CurrentSelection(), nil); err != contextbuild.ErrNoCompactableHistory || !recoverableCompaction(err) || requests != 0 || r.Current() != before || r.checkContext() != nil {
		t.Fatal("no-progress compaction consumed fresh results or invalidated context", err, requests)
	}
	retained, err := r.Store.Messages(before)
	if err != nil || !reflect.DeepEqual(retained, original) {
		t.Fatal("no-progress compaction changed durable results", err)
	}
}
