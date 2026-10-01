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
