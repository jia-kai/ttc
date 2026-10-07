package session

import (
	"context"
	"reflect"
	"testing"

	"ttc/internal/llm"
)

func TestUsageAcrossToolRoundsAndTurns(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	cached := []int{15, 70, 120}
	reasoning := []int{3, 10, 0}
	usages := []llm.Usage{
		{InputTokens: 100, OutputTokens: 10, CachedInputTokens: &cached[0], ReasoningOutputTokens: &reasoning[0]},
		{InputTokens: 200, OutputTokens: 40, CachedInputTokens: &cached[1], ReasoningOutputTokens: &reasoning[1]},
		{InputTokens: 300, OutputTokens: 70, CachedInputTokens: &cached[2], ReasoningOutputTokens: &reasoning[2]},
	}
	rounds := 0
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		round := rounds
		rounds++
		if round >= len(usages) {
			t.Fatal("unexpected extra inference round", rounds)
		}
		if round == 0 {
			call := llm.ToolCall{ID: "read", Name: "read", Arguments: []byte(`{"path":"."}`)}
			if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
				return err
			}
		} else if err := emit(llm.StreamEvent{Kind: "text", Text: "Done."}); err != nil {
			return err
		}
		return emit(llm.StreamEvent{Kind: "completed", Usage: &usages[round]})
	}}
	for turn, want := range []struct {
		rounds, input, cached, output, reasoning int
	}{
		{2, 300, 85, 50, 13},
		{3, 600, 205, 120, 13},
	} {
		if err := r.Run(&llm.Message{Role: "user", Content: "Continue"}); err != nil {
			t.Fatal(err)
		}
		got := r.UsageSnapshot()
		if rounds != want.rounds || got.Reported == nil || !reflect.DeepEqual(got.Reported.Tokens, usages[rounds-1]) {
			t.Fatalf("turn %d: last-round usage = %+v, rounds = %d", turn, got.Reported, rounds)
		}
		totals := got.Totals
		if totals.Requests != want.rounds || totals.ReportedRequests != want.rounds || totals.Tokens.InputTokens != want.input || totals.Tokens.CachedInputTokens == nil || *totals.Tokens.CachedInputTokens != want.cached || totals.Tokens.OutputTokens != want.output || totals.Tokens.ReasoningOutputTokens == nil || *totals.Tokens.ReasoningOutputTokens != want.reasoning {
			t.Fatalf("turn %d: accumulated usage = %+v (%+v), want %+v", turn, totals, totals.Tokens, want)
		}
	}
}
