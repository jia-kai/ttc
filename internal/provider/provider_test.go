package provider

import (
	"context"
	"testing"
)

func TestResolveRejectsUnknownAndInvalidBudgets(t *testing.T) {
	m := ScriptModel()
	if _, e := Resolve("test", []ModelSpec{m}, m.ID, "missing"); e == nil {
		t.Fatal("accepted variant")
	}
	if _, e := Resolve("test", []ModelSpec{m}, "missing", ""); e == nil {
		t.Fatal("accepted model")
	}
	m.Budget.OutputAllowance = m.Budget.ContextLimit
	if _, e := Resolve("test", []ModelSpec{m}, m.ID, ""); e == nil {
		t.Fatal("accepted impossible budget")
	}
	m = ScriptModel()
	m.Budget.MaxOutputTokens = 0
	if e := m.Budget.Validate(); e != nil {
		t.Fatal(e)
	}
}

func TestBudgetRejectsInvalidRecentCycleBounds(t *testing.T) {
	for _, bounds := range [][2]int{{-1, 100}, {0, 0}, {101, 100}} {
		budget := ScriptModel().Budget
		budget.RecentTokensMin, budget.RecentTokensMax = bounds[0], bounds[1]
		if err := budget.Validate(); err == nil {
			t.Fatal("accepted invalid retention bounds", bounds)
		}
	}
}

func TestScriptCancellationAndExhaustion(t *testing.T) {
	s := &Script{Responses: []ScriptResponse{{Text: "hello"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := s.Stream(ctx, Request{}, func(StreamEvent) error { return nil }); e == nil {
		t.Fatal("ignored cancellation")
	}
	if e := s.Stream(context.Background(), Request{}, func(StreamEvent) error { return nil }); e == nil {
		t.Fatal("ignored exhaustion")
	}
}
