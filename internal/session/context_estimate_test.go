package session

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/providers/openai"
)

type fixturePreparedInput struct{}

func (*fixturePreparedInput) Provider() string { return "script" }

// estimateUsage is a fixture helper for constructing conservative budgets. Runtime
// admission and handoff checks always delegate the complete request to Inference.
func estimateUsage(selection llm.Selection, system string, defs []llm.ToolDefinition, messages []llm.Message) ContextUsage {
	messages, err := contextbuild.ProjectRetainedInputs(llm.ContextFor(selection, messages))
	if err != nil {
		panic(err)
	}
	result, err := llm.EstimateInput(context.Background(), llm.Request{Selection: selection, System: system, Tools: defs, Messages: messages})
	if err != nil {
		panic(err)
	}
	return usageFromEstimate(selection, result)
}

func TestManualCompactionRefreshesOccupancyWithoutReplacingReportedUsage(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("Earlier notes. ", 700))
	if _, err := r.Store.Append(r.Current(), "", "main", "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: "Continue."}); err != nil {
		t.Fatal(err)
	}
	budget := r.selection.Model.Budget
	predecessorInput := budget.ContextLimit - budget.OutputAllowance - budget.EstimationMargin - budget.NextTurnInputReserve
	r.usage = ContextUsage{Input: predecessorInput, Limit: budget.ContextLimit}
	r.reported = &ReportedUsage{Model: "previous", Tokens: llm.Usage{InputTokens: predecessorInput, OutputTokens: 50}}
	r.recordUsage(&r.reported.Tokens)
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Earlier task completed."}); err != nil {
			return err
		}
		return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 4000, OutputTokens: 80}})
	}}
	if _, err := r.compact("", false); err != nil {
		t.Fatal(err)
	}
	u := r.UsageSnapshot()
	if !u.Estimated || u.Input >= predecessorInput || u.Input <= 0 || u.Reported.Tokens.InputTokens != predecessorInput {
		t.Fatal("handoff retained occupancy or replaced response", u)
	}
	if u.Totals.Requests != 2 || u.Totals.Tokens.InputTokens != predecessorInput+4000 || u.Totals.Tokens.OutputTokens != 130 {
		t.Fatal("compaction usage duplicated", u.Totals)
	}
}

func TestMainRequestFeedbackUpdatesDisplayedUsage(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 1234}})
	}}
	if _, err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Measure this request."}); err != nil {
		t.Fatal(err)
	}
	u := r.UsageSnapshot()
	if u.Input != 1234 || u.Estimated || u.Reported == nil || u.Reported.Tokens.InputTokens != 1234 {
		t.Fatal("report not reflected in status", u)
	}
}

func TestAdmissionDelegatesCompleteInputAndPropagatesEstimateErrors(t *testing.T) {
	for _, role := range []string{"main", "child", "aside"} {
		t.Run(role, func(t *testing.T) {
			a := newSummaryBudgetActor(t, role)
			want := a.snapshot(t)
			failure := errors.New("provider estimate unavailable")
			calls := 0
			a.r.Provider = &childProvider{estimate: func(_ context.Context, req llm.Request) (llm.InputEstimate, error) {
				calls++
				if !reflect.DeepEqual(req, want) {
					t.Fatal("admission estimated an incomplete or different request", req, want)
				}
				return llm.InputEstimate{Input: -1}, failure
			}}
			before, err := a.r.Store.Session(a.r.Current())
			if err != nil {
				t.Fatal(err)
			}
			admitted, err := a.admit()
			if !errors.Is(err, failure) || admitted.RequestID != 0 || calls != 1 {
				t.Fatal(admitted, err, calls)
			}
			after, err := a.r.Store.Session(a.r.Current())
			if err != nil || after.EntryTip != before.EntryTip {
				t.Fatal("estimate failure mutated history", after, err)
			}
		})
	}
}

func TestCompactionEstimationErrorsAreNotCapacityRefusals(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Research")
	failure := errors.New("provider preparation failed")
	r.Provider = &childProvider{estimate: func(context.Context, llm.Request) (llm.InputEstimate, error) {
		return llm.InputEstimate{Input: -1}, failure
	}}
	request := llm.Request{ConversationID: r.Current(), Selection: r.selection, Messages: []llm.Message{{Type: llm.TypeUserInput, Content: "Research"}}}
	if _, err := r.summarize(context.Background(), "main", "", request, "", false); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := r.compactionEstimate(context.Background(), r.Current(), r.selection, systemTemplate, r.Tools.Definitions(), request.Messages, nil, nil); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestAsideAdmissionDoesNotTransferMainMeasurementAcrossHeaders(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Inherited research.")
	provider := &openai.Adapter{}
	r.Provider = provider
	r.selection.Provider = "openai"
	ctx := context.Background()
	prefix, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	mainRequest := llm.Request{ConversationID: r.Current(), Selection: r.selection, System: systemTemplate, Tools: r.Tools.Definitions(), Messages: prefix}
	mainEstimate, err := r.estimateInput(ctx, mainRequest)
	if err != nil || mainEstimate.Checkpoint == nil {
		t.Fatal(mainEstimate, err)
	}
	if err := provider.RestoreEstimate(*mainEstimate.Checkpoint, llm.Usage{InputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	measured, err := r.estimateInput(ctx, mainRequest)
	if err != nil || measured.Input != 1 || measured.Estimated {
		t.Fatal("main measurement was not restored", measured, err)
	}
	actor := "main/btw_strict_header"
	turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
	if err != nil {
		t.Fatal(err)
	}
	tools := r.Tools.Filter(readOnlyTool)
	messages := append(append([]llm.Message(nil), prefix...), llm.Message{Type: llm.TypeRuntimeInstruction, Content: btwInstruction}, llm.Message{Type: llm.TypeUserInput, Content: "Explain the research.", InputSource: "btw", InputTimeMS: 1000})
	cm, _, err := r.runtimeContext(ctx, actor, r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	input := append(append([]llm.Message(nil), messages...), *cm)
	asideRequest := llm.Request{ConversationID: actor, Selection: r.selection, System: asideSystemTemplate, Tools: tools.Definitions(), Messages: input}
	asideEstimate, err := r.estimateInput(ctx, asideRequest)
	if err != nil || !asideEstimate.Estimated || asideEstimate.Input <= 1 {
		t.Fatal("aside borrowed main measurement across changed system/tools", asideEstimate, err)
	}
	selection := r.selection
	b := &selection.Model.Budget
	b.ContextLimit = asideEstimate.Input + b.OutputAllowance + b.EstimationMargin + b.SummaryOutputAllowance + compactionInstructionTokens()
	if _, _, err := r.admitChild(ctx, childTask{actor: actor, turn: turn, selection: selection, tools: tools, aside: true}, messages, contextCursor{}); !errors.Is(err, errNeedsCompaction) {
		t.Fatal("aside bypassed strict capacity boundary using main evidence", err)
	}
}

func TestActorStreamUsesExactPreparedAdmission(t *testing.T) {
	for _, role := range []string{"main", "child", "aside"} {
		t.Run(role, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			prepared := &fixturePreparedInput{}
			var estimated llm.Request
			estimates, streams := 0, 0
			r.Provider = &childProvider{
				estimate: func(_ context.Context, req llm.Request) (llm.InputEstimate, error) {
					estimates++
					estimated = req
					estimated.Prepared = prepared
					return llm.InputEstimate{Input: 100, Estimated: true, Prepared: prepared}, nil
				},
				stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
					streams++
					req.Observe = nil
					if len(req.Messages) != len(estimated.Messages) {
						t.Fatal("stream changed prepared message count")
					}
					for i, got := range req.Messages {
						want := estimated.Messages[i]
						// History commit fills audit identity and human timestamps;
						// neither changes this non-retained native input.
						got.RequestID, got.InputTimeMS = want.RequestID, want.InputTimeMS
						if !reflect.DeepEqual(got, want) {
							t.Fatal("stream changed prepared model content", got, want)
						}
					}
					estimated.Messages = req.Messages
					if req.Prepared != prepared || !reflect.DeepEqual(req, estimated) {
						t.Fatal("stream reconstructed or lost prepared admission input")
					}
					return emit(llm.StreamEvent{Kind: "text", Text: "Done."})
				},
			}
			if role == "main" {
				if _, err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Research."}); err != nil {
					t.Fatal(err)
				}
			} else {
				seedRuntime(t, r, "Parent task.")
				actor := "main/child_prepared"
				if role == "aside" {
					actor = "main/btw_prepared"
				}
				turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.runChild(context.Background(), childTask{actor: actor, turn: turn, prompt: "Research.", selection: r.selection, tools: r.Tools.Filter(readOnlyTool), aside: role == "aside"}, io.Discard, io.Discard); err != nil {
					t.Fatal(err)
				}
			}
			if estimates != 1 || streams != 1 {
				t.Fatal("unexpected preparation/inference count", estimates, streams)
			}
		})
	}
}
