package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	contextbuild "scicode/internal/context"
	"scicode/internal/provider"
	"scicode/internal/provider/openai"
)

func TestMainAndChildAdmissionUseAdapterReplayEstimate(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	r.Provider = &openai.Adapter{}
	r.selection.Provider = "openai"
	seedRuntime(t, r, "Continue reasoning.")
	item, err := json.Marshal(map[string]string{"type": "reasoning", "encrypted_content": strings.Repeat("A", 12000)})
	if err != nil {
		t.Fatal(err)
	}
	m := provider.Message{Role: "assistant", State: &provider.ReplayState{Provider: "openai", Model: r.selection.Model.RequestID(), Version: 1, Items: []json.RawMessage{item}}}
	if _, err := r.Store.Append(r.Current(), "", "main", "message", "assistant", true, m); err != nil {
		t.Fatal(err)
	}
	cm, _, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	base := estimateUsage(r.selection, systemTemplate, r.Tools.Definitions(), []provider.Message{messages[0], *cm})
	r.selection.Model.Budget.ContextLimit = base.Input + base.Reserved + 3000
	if contextbuild.Fits(r.selection, systemTemplate, r.Tools.Definitions(), append(messages, *cm), false) {
		t.Fatal("fixture did not expose transport inflation")
	}
	turn := admissionTurn(t, r)
	if _, _, err := r.admitMain(context.Background(), turn, r.selection); err != nil {
		t.Fatal("main compacted due to transport size", err)
	}
	u := r.UsageSnapshot()
	annotated := r.contextMessages(r.selection, messages)
	if u.Input != base.Input+contextbuild.Tokens(annotated[1:]) {
		t.Fatal("sidebar and admission disagree", u.Input, base.Input, contextbuild.Tokens(annotated[1:]))
	}
	if messages[1].State.EstimatedTokens != nil {
		t.Fatal("estimation mutated stored projection")
	}
	encoded, err := json.Marshal(annotated[1])
	if err != nil || strings.Contains(string(encoded), "EstimatedTokens") || strings.Contains(string(encoded), "estimated_tokens") {
		t.Fatal("derived estimate leaked into durable state", err)
	}
	actor := "main/child_estimate"
	childTurn, err := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
	if err != nil {
		t.Fatal(err)
	}
	childMessages := []provider.Message{{Role: "user", Content: "Continue reasoning."}, m}
	if _, _, err := r.admitChild(context.Background(), childTask{actor: actor, turn: childTurn, selection: r.selection, tools: r.Tools}, childMessages, contextCursor{}); err != nil {
		t.Fatal("child compacted due to transport size", err)
	}
}

func TestManualCompactionRefreshesOccupancyWithoutReplacingReportedUsage(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("Earlier notes. ", 700))
	if _, err := r.Store.Append(r.Current(), "", "main", "message", "user", true, provider.Message{Role: "user", Content: "Continue."}); err != nil {
		t.Fatal(err)
	}
	r.usage = ContextUsage{RequestID: 87, Input: 9000, Limit: r.selection.Model.Budget.ContextLimit}
	r.reported = &ReportedUsage{RequestID: 87, Model: "previous", Tokens: provider.Usage{InputTokens: 9000, OutputTokens: 50}}
	r.recordUsage(&r.reported.Tokens)
	r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		if err := emit(provider.StreamEvent{Kind: "text", Text: "Earlier task completed."}); err != nil {
			return err
		}
		return emit(provider.StreamEvent{Kind: "completed", Usage: &provider.Usage{InputTokens: 4000, OutputTokens: 80}})
	}}
	if _, err := r.compact(""); err != nil {
		t.Fatal(err)
	}
	u := r.UsageSnapshot()
	if u.RequestID != 0 || u.Input >= 9000 || u.Input <= 0 || u.Reported.RequestID != 87 {
		t.Fatal("manual handoff kept predecessor occupancy or replaced response", u)
	}
	if u.Totals.Requests != 2 || u.Totals.Tokens.InputTokens != 13000 || u.Totals.Tokens.OutputTokens != 130 {
		t.Fatal("compaction usage was duplicated or became context size", u.Totals)
	}
}
