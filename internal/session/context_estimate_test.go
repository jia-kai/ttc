package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/providers/openai"
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
	m := llm.Message{Type: llm.TypeAssistant, State: &llm.ReplayState{Provider: "openai", Model: r.selection.Model.RequestID(), Version: 1, Items: []json.RawMessage{item}}}
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
	base := estimateUsage(r.selection, systemTemplate, r.Tools.Definitions(), []llm.Message{messages[0], *cm})
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
	childMessages := []llm.Message{{Type: llm.TypeUserInput, Content: "Continue reasoning."}, m}
	if _, _, err := r.admitChild(context.Background(), childTask{actor: actor, turn: childTurn, selection: r.selection, tools: r.Tools, estimator: &contextbuild.Estimator{}}, childMessages, contextCursor{}); err != nil {
		t.Fatal("child compacted due to transport size", err)
	}
}

func TestManualCompactionRefreshesOccupancyWithoutReplacingReportedUsage(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("Earlier notes. ", 700))
	if _, err := r.Store.Append(r.Current(), "", "main", "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: "Continue."}); err != nil {
		t.Fatal(err)
	}
	// Keep the predecessor near the model's input ceiling, independent of the
	// size of the current generated tool and runtime prompts.
	budget := r.selection.Model.Budget
	predecessorInput := budget.ContextLimit - budget.OutputAllowance - budget.EstimationMargin - budget.NextTurnInputReserve
	r.usage = ContextUsage{Input: predecessorInput, Limit: budget.ContextLimit}
	r.reported = &ReportedUsage{Model: "previous", Tokens: llm.Usage{InputTokens: predecessorInput, OutputTokens: 50}}
	oldSnapshot := contextbuild.InputSnapshot{Selection: r.selection, System: systemTemplate, Tools: r.Tools.Definitions(), Lineage: r.Current(), ReplayEstimator: r.Provider}
	r.estimator.ObserveRequest(87, r.estimator.Prepare(oldSnapshot))
	r.estimator.ObserveUsage(87, predecessorInput)
	r.recordUsage(&r.reported.Tokens)
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Earlier task completed."}); err != nil {
			return err
		}
		return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 4000, OutputTokens: 80}})
	}}
	if _, err := r.compact(""); err != nil {
		t.Fatal(err)
	}
	u := r.UsageSnapshot()
	if !u.Estimated || u.Input >= predecessorInput || u.Input <= 0 || u.Reported.Tokens.InputTokens != predecessorInput {
		t.Fatal("manual handoff kept predecessor occupancy or replaced response", u)
	}
	if u.Totals.Requests != 2 || u.Totals.Tokens.InputTokens != predecessorInput+4000 || u.Totals.Tokens.OutputTokens != 130 {
		t.Fatal("compaction usage was duplicated or became context size", u.Totals)
	}
	if result := r.estimator.Estimate(oldSnapshot); !result.Estimated || result.Input != result.LocalInput {
		t.Fatal("handoff retained measured anchor", result)
	}
}

func TestMainAdmissionUsesMeasuredInputAtThreshold(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Continue research.")
	turn := admissionTurn(t, r)
	first, _, err := r.admitMain(context.Background(), turn, r.selection)
	if err != nil {
		t.Fatal(err)
	}
	local := r.UsageSnapshot()
	if local.Input+local.Reserved >= local.Limit {
		t.Fatal("fixture does not fit locally", local)
	}
	if _, ok := r.estimator.ObserveUsage(first.RequestID, local.Limit-local.Reserved); !ok {
		t.Fatal("provider feedback rejected")
	}
	if _, _, err := r.admitMain(context.Background(), turn, r.selection); err != errNeedsCompaction {
		t.Fatalf("measured input at threshold admitted: %v", err)
	}
	u := r.UsageSnapshot()
	if u.Input+u.Reserved != u.Limit {
		t.Fatal("admission and displayed occupancy disagree", u)
	}
	r.estimator.Reset()
	if _, _, err := r.admitMain(context.Background(), turn, r.selection); err != nil {
		t.Fatal("reset retained old measurement", err)
	}
}

func TestMainRequestFeedbackUpdatesSharedEstimate(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 1234, OutputTokens: 0}})
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Measure this request."}); err != nil {
		t.Fatal(err)
	}
	u := r.UsageSnapshot()
	if u.Input != 1234 || u.Estimated || u.Reported == nil || u.Reported.Tokens.InputTokens != 1234 {
		t.Fatal("report not reflected in shared estimate", u)
	}
}

func TestMainInterruptedFeedbackPreservesAnchorAndModelSwitchResetsIt(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	var baseline contextbuild.InputSnapshot
	interrupted := errors.New("interrupted after usage")
	calls := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		calls++
		if calls == 1 {
			baseline = contextbuild.InputSnapshot{Selection: req.Selection, System: req.System, Tools: req.Tools, Messages: req.Messages, Lineage: r.Current(), ReplayEstimator: r.Provider}
			return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 1234}})
		}
		if err := emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 9999}}); err != nil {
			return err
		}
		return interrupted
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "First request."}); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Interrupted request."}); !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	if result := r.estimator.Estimate(baseline); result.Input != 1234 || result.Estimated {
		t.Fatal("failed stream replaced successful measurement", result)
	}
	selection := r.CurrentSelection()
	selection.Model.ID += "-other"
	if err := r.RequestModel(selection); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyModel(""); err != nil {
		t.Fatal(err)
	}
	if result := r.estimator.Estimate(baseline); !result.Estimated || result.Input != result.LocalInput {
		t.Fatal("model switch retained old measurement", result)
	}
}
