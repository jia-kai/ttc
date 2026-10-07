package llm

import (
	"context"
	"strings"
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

func TestChoiceTextValidation(t *testing.T) {
	for _, limit := range []int{MaxProviderIDBytes, MaxModelIDBytes, MaxVariantBytes} {
		for _, value := range []string{"a", "模型", strings.Repeat("a", limit)} {
			if err := ValidateChoiceText("choice", value, limit); err != nil {
				t.Fatalf("valid choice %q: %v", value, err)
			}
		}
		for _, value := range []string{"", " a", "a ", "\u2003a", "a\n", "a\x00b", "a\u0085b", string([]byte{0xff}), strings.Repeat("a", limit+1)} {
			if err := ValidateChoiceText("choice", value, limit); err == nil {
				t.Fatalf("accepted invalid choice %q", value)
			}
		}
	}
}

func TestBudgetReserveSumCannotOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, reserves := range [][4]int{{maxInt, 1, 1, 1}, {1, maxInt, 1, 1}, {1, 1, maxInt, 1}, {1, 1, 1, maxInt}, {maxInt / 2, maxInt / 2, 1, 1}} {
		budget := ScriptModel().Budget
		budget.ContextLimit, budget.MaxOutputTokens = maxInt, 0
		budget.OutputAllowance, budget.EstimationMargin, budget.NextTurnInputReserve, budget.SummaryOutputAllowance = reserves[0], reserves[1], reserves[2], reserves[3]
		if err := budget.Validate(); err == nil || !strings.Contains(err.Error(), "reserves exceed capacity") {
			t.Fatalf("overflowing reserves accepted: %v: %v", reserves, err)
		}
	}
	budget := ScriptModel().Budget
	budget.ContextLimit, budget.MaxOutputTokens = maxInt, 0
	budget.OutputAllowance = maxInt - budget.EstimationMargin - budget.NextTurnInputReserve - budget.SummaryOutputAllowance - 1
	if err := budget.Validate(); err != nil {
		t.Fatal("valid near-limit budget rejected", err)
	}
	budget.OutputAllowance++
	if err := budget.Validate(); err == nil {
		t.Fatal("accepted reserves consuming all context")
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
