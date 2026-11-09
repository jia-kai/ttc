package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// summaryBudgetActor retains the isolated actor's history and delivery cursor.
type summaryBudgetActor struct {
	r        *Runtime
	turn     string
	task     *childTask
	messages []llm.Message
	cursor   contextCursor
}

func TestCodingInputFitsReservesInstructionAndSummaryAtStrictBoundary(t *testing.T) {
	directive := llm.Message{Type: llm.TypeRuntimeInstruction, Content: prompts.Compaction}
	if got := compactionInstruction(); !reflect.DeepEqual(got, directive) {
		t.Fatalf("compaction instruction = %+v, want %+v", got, directive)
	}
	d := contextbuild.Tokens([]llm.Message{directive})
	if got := compactionInstructionTokens(); got != d || d <= 0 {
		t.Fatalf("instruction tokens = %d, want %d", got, d)
	}
	for _, input := range []int{0, 113, 4097} {
		for _, headroom := range []bool{false, true} {
			for _, extra := range []int{-1, 0, 1} {
				t.Run(fmt.Sprintf("input=%d/headroom=%v/extra=%d", input, headroom, extra), func(t *testing.T) {
					b := llm.Budget{OutputAllowance: 701, SummaryOutputAllowance: 1301, EstimationMargin: 307, NextTurnInputReserve: 509}
					b.ContextLimit = input + b.OutputAllowance + b.EstimationMargin + b.SummaryOutputAllowance + d + extra
					if headroom {
						b.ContextLimit += b.NextTurnInputReserve
					}
					if fits := codingInputFits(input, b, headroom); fits != (extra > 0) {
						t.Fatalf("input=%d budget=%+v: fits=%v", input, b, fits)
					}
				})
			}
		}
	}
}

func TestCodingInputFitsRejectsInvalidAndOverflowingBudgets(t *testing.T) {
	valid := llm.ScriptModel().Budget
	for _, field := range []string{"input", "context", "output", "summary", "margin", "next"} {
		t.Run(field, func(t *testing.T) {
			input, b := 17, valid
			switch field {
			case "input":
				input = -1
			case "context":
				b.ContextLimit = -1
			case "output":
				b.OutputAllowance = -1
			case "summary":
				b.SummaryOutputAllowance = -1
			case "margin":
				b.EstimationMargin = -1
			case "next":
				b.NextTurnInputReserve = -1
			}
			if codingInputFits(input, b, true) {
				t.Fatal("invalid input/budget fit", input, b)
			}
		})
	}
	maxInt := int(^uint(0) >> 1)
	if codingInputFits(maxInt, valid, false) {
		t.Fatal("overflowing input fit")
	}
	valid.ContextLimit, valid.SummaryOutputAllowance = maxInt, maxInt
	if codingInputFits(1, valid, true) {
		t.Fatal("overflowing reserves fit")
	}
}

func TestCompactionHandoffUsesProviderEstimateAndNextTurnHeadroom(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			selection := r.selection
			b := &selection.Model.Budget
			b.ContextLimit = 123 + b.OutputAllowance + b.EstimationMargin + b.SummaryOutputAllowance + compactionInstructionTokens() + b.NextTurnInputReserve + extra
			retained := []llm.Message{llm.NewCompactionContext("Summary handoff.")}
			notices := []llm.Message{{Type: llm.TypeRuntimeNotification, Content: `{"type":"job_exit","job_id":"pending"}`}}
			runtime := &llm.Message{Type: llm.TypeRuntimeContext, Content: "live context"}
			want := append(append(append([]llm.Message(nil), retained...), notices...), *runtime)
			r.Provider = &childProvider{estimate: func(_ context.Context, req llm.Request) (llm.InputEstimate, error) {
				if req.ConversationID != "actor" || req.System != "system" || !reflect.DeepEqual(req.Messages, want) {
					t.Fatal("incomplete handoff input", req)
				}
				return llm.InputEstimate{Input: 123, Estimated: true}, nil
			}}
			estimate, err := r.compactionEstimate(context.Background(), "actor", selection, "system", nil, retained, notices, runtime)
			if err != nil || codingInputFits(estimate.Input, *b, true) != (extra > 0) {
				t.Fatal(estimate, err)
			}
		})
	}
}

func TestIsolatedCompactionHandoffBudgetsPendingGuidanceAndRuntimeNotices(t *testing.T) {
	for _, role := range []string{"child", "aside"} {
		t.Run(role, func(t *testing.T) {
			a := newSummaryBudgetActor(t, role)
			a.append(t, llm.Message{Type: llm.TypeAssistant, Content: strings.Repeat("Older isolated evidence. ", 700)})
			assignment := a.task.assignment
			guidanceJSON, err := json.Marshal(map[string]string{"message": strings.Repeat("Pending parent guidance. ", 200)})
			if err != nil {
				t.Fatal(err)
			}
			guidance := llm.Message{Type: llm.TypeChildGuidance, Content: string(guidanceJSON)}
			assignment.queued = []llm.Message{guidance}
			assignment.queuedTokens = noticeTokens(guidance)
			source, err := a.r.Store.Append(a.r.Current(), "", a.task.actor, "status", "", false, map[string]any{"type": "runtime_event", "body": map[string]string{"type": "job_exit", "job_id": "pending_handoff"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := assignment.runtimeNotices.enqueue(source, ""); err != nil {
				t.Fatal(err)
			}
			notices, _, err := a.r.childNoticeBatchLocked(assignment)
			if err != nil || len(notices) != 2 {
				t.Fatal(notices, err)
			}
			prepared, err := prepareCompaction(a.messages, *a.selection())
			if err != nil {
				t.Fatal(err)
			}
			const summary = "Earlier isolated evidence summarized."
			replacement := []llm.Message{llm.NewCompactionContext(summary)}
			if a.task.aside {
				replacement = append(replacement, llm.Message{Type: llm.TypeRuntimeInstruction, Content: btwInstruction})
			}
			replacement = append(replacement, prepared.inputs...)
			replacement = append(replacement, prepared.messages[prepared.retention.Start:]...)
			cm, _, err := a.r.runtimeContext(context.Background(), a.task.actor, *a.selection(), contextCursor{})
			if err != nil {
				t.Fatal(err)
			}
			system := childSystemTemplate
			if a.task.aside {
				system = asideSystemTemplate
			}
			required := func(pending []llm.Message) int {
				messages := append(append(append([]llm.Message(nil), replacement...), pending...), *cm)
				input := estimateUsage(*a.selection(), system, a.r.Tools.Definitions(), messages)
				return input.Input + input.Reserved + a.selection().Model.Budget.NextTurnInputReserve
			}
			without, with := required(nil), required(notices)
			a.selection().Model.Budget.ContextLimit = without + (with-without)/2
			before, cursor, queuedTokens := a.r.Current(), a.cursor, assignment.queuedTokens
			summaries := 0
			a.r.Provider = &childProvider{
				estimate: func(ctx context.Context, req llm.Request) (llm.InputEstimate, error) {
					if req.NoTools {
						return llm.InputEstimate{Input: 256, Estimated: true}, nil
					}
					return llm.EstimateInput(ctx, req)
				},
				stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
					summaries++
					return emit(llm.StreamEvent{Kind: "text", Text: summary})
				},
			}
			result, next, err := a.r.compactChild(context.Background(), *a.task, a.messages, cursor, nil)
			var capacity *compactionCapacityError
			if !errors.As(err, &capacity) || !recoverableCompaction(err) || result != nil || summaries != 1 {
				t.Fatal(result, next, err, summaries)
			}
			pending := pendingChildNotices(t, a.r, assignment)
			if a.r.Current() != before || !reflect.DeepEqual(cursor, next) || !reflect.DeepEqual(assignment.queued, []llm.Message{guidance}) || assignment.queuedTokens != queuedTokens || len(pending) != 1 || pending[0].EventSeq != source {
				t.Fatal("failed handoff consumed delivery or changed context")
			}
			var successes, acknowledged int
			if err := historytest.DB(t, a.r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='actor_compacted' OR json_extract(content_json,'$.body.type')='child_compacted'`).Scan(&successes); err != nil || successes != 0 {
				t.Fatal(successes, err)
			}
			if err := historytest.DB(t, a.r.Store).QueryRow(`SELECT count(*) FROM entries WHERE id=? AND delivered_request_id IS NOT NULL`, source).Scan(&acknowledged); err != nil || acknowledged != 0 {
				t.Fatal(acknowledged, err)
			}
		})
	}
}

func TestSummarizePreflightUsesProviderCompleteRequest(t *testing.T) {
	for _, role := range []string{"main", "child", "aside"} {
		for _, extra := range []int{-1, 0, 1} {
			t.Run(fmt.Sprintf("%s/%d", role, extra), func(t *testing.T) {
				a := newSummaryBudgetActor(t, role)
				snapshot := a.snapshot(t)
				b := &snapshot.Selection.Model.Budget
				b.ContextLimit = 256 + b.SummaryOutputAllowance + b.EstimationMargin + extra
				var estimated llm.Request
				prepared := &fixturePreparedInput{}
				calls := 0
				a.r.Provider = &childProvider{
					estimate: func(_ context.Context, req llm.Request) (llm.InputEstimate, error) {
						estimated = req
						estimated.Prepared = prepared
						if !req.NoTools || req.OutputTokens != b.SummaryOutputAllowance || req.System != snapshot.System || !reflect.DeepEqual(req.Tools, snapshot.Tools) || req.ConversationID != snapshot.ConversationID {
							t.Fatal("summary estimate omitted request options", req)
						}
						want := append(append([]llm.Message(nil), snapshot.Messages...), llm.Message{Type: llm.TypeRuntimeInstruction, Content: fmt.Sprintf(prompts.CompactionFocus, "focus")}, compactionInstruction())
						if !reflect.DeepEqual(req.Messages, want) {
							t.Fatal("summary estimate omitted focus/directive")
						}
						return llm.InputEstimate{Input: 256, Estimated: true, Prepared: prepared}, nil
					},
					stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
						calls++
						req.Observe = nil // Inspection is attached only at the stream boundary.
						if !reflect.DeepEqual(req, estimated) {
							t.Fatal("summary sent different input than estimated")
						}
						return emit(llm.StreamEvent{Kind: "text", Text: "Summary."})
					},
				}
				actor := "main"
				if a.task != nil {
					actor = a.task.actor
				}
				text, err := a.r.summarize(context.Background(), actor, a.turn, snapshot, "focus", false)
				if extra > 0 {
					if err != nil || text != "Summary." || calls != 1 {
						t.Fatal(text, err, calls)
					}
				} else {
					var capacity *compactionCapacityError
					if !errors.As(err, &capacity) || calls != 0 {
						t.Fatal(err, calls)
					}
				}
			})
		}
	}
}

func TestSummaryBudgetCompactsBeforeCodingConsumesFreshResults(t *testing.T) {
	a := newSummaryBudgetActor(t, "main")
	first, err := a.admit()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.r.Store.FinishRequest(first.RequestID, history.RequestCompletion{Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := a.r.Store.FinishTurn(a.turn, "completed"); err != nil {
		t.Fatal(err)
	}
	a.append(t, llm.Message{Type: llm.TypeAssistant, Content: strings.Repeat("Older canonical evidence. ", 2500)})
	fresh := []llm.Message{
		{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "fresh-a", Name: "read", Arguments: json.RawMessage(`{"path":"fresh-a.txt"}`)}, {ID: "fresh-b", Name: "read", Arguments: json.RawMessage(`{"path":"fresh-b.txt"}`)}}},
		{Type: llm.TypeToolResult, CallID: "fresh-a", Content: strings.Repeat("UNCONSUMED_A\n", 250)},
		{Type: llm.TypeToolResult, CallID: "fresh-b", Content: strings.Repeat("UNCONSUMED_B\n", 250)},
	}
	for _, message := range fresh {
		a.append(t, message)
	}
	snapshot := a.snapshot(t)
	estimate, err := a.r.estimateInput(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	b := &a.selection().Model.Budget
	b.RecentTokensMin, b.RecentTokensMax = 0, 100
	b.ContextLimit = estimate.Input + b.OutputAllowance + b.EstimationMargin + b.SummaryOutputAllowance + compactionInstructionTokens()
	if !contextbuild.FitsInput(estimate.Input, *b, false) || codingInputFits(estimate.Input, *b, false) {
		t.Fatal("fixture must fit coding alone but require summary-safe compaction", estimate, *b)
	}
	before := a.r.Current()
	summaries, coding := 0, 0
	a.r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if req.NoTools {
			summaries++
			var contents strings.Builder
			for _, message := range req.Messages {
				contents.WriteString(message.Content)
				for _, call := range message.Calls {
					contents.Write(call.Arguments)
				}
			}
			if coding != 0 || !strings.Contains(contents.String(), "Older canonical evidence.") || strings.Contains(contents.String(), "UNCONSUMED_") || strings.Contains(contents.String(), "fresh-a.txt") {
				t.Fatal("summary must precede consumption and exclude the fresh cycle")
			}
			if !reflect.DeepEqual(req.Messages[len(req.Messages)-1], compactionInstruction()) {
				t.Fatal("summary did not end with full directive")
			}
			return emit(llm.StreamEvent{Kind: "text", Text: "Older evidence summarized; inspect the fresh results next."})
		}
		coding++
		if summaries != 1 || a.r.Current() == before {
			t.Fatal("coding consumed fresh results before summary-safe compaction")
		}
		var retained []llm.Message
		for _, message := range req.Messages {
			if len(message.Calls) > 0 || message.Type == llm.TypeToolResult {
				retained = append(retained, message)
			}
		}
		if !reflect.DeepEqual(retained, fresh) {
			t.Fatal("handoff changed or omitted fresh calls/results")
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Fresh results consumed after compaction."})
	}}
	if _, err := a.r.Run(nil); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 || coding != 1 {
		t.Fatal("unexpected inference sequence", summaries, coding)
	}
}

func TestCodingAdmissionUsesProviderInputWithInstructionAndSummaryReserve(t *testing.T) {
	for _, role := range []string{"main", "child", "aside"} {
		for _, extra := range []int{-1, 0, 1} {
			t.Run(fmt.Sprintf("%s/%d", role, extra), func(t *testing.T) {
				a := newSummaryBudgetActor(t, role)
				b := &a.selection().Model.Budget
				reserved := b.OutputAllowance + b.EstimationMargin + b.SummaryOutputAllowance + compactionInstructionTokens()
				b.ContextLimit = 256 + reserved + extra
				calls := 0
				a.r.Provider = &childProvider{estimate: func(_ context.Context, req llm.Request) (llm.InputEstimate, error) {
					calls++
					if req.System == "" || len(req.Tools) == 0 || len(req.Messages) == 0 {
						t.Fatal("incomplete coding estimate", req)
					}
					return llm.InputEstimate{Input: 256, Estimated: false}, nil
				}}
				cursor := a.r.mainContext
				admitted, err := a.admit()
				if calls != 1 {
					t.Fatal("admission did not delegate once", calls)
				}
				if extra > 0 {
					if err != nil || admitted.RequestID == 0 {
						t.Fatal(admitted, err)
					}
				} else if !errors.Is(err, errNeedsCompaction) || admitted.RequestID != 0 || !reflect.DeepEqual(cursor, a.r.mainContext) {
					t.Fatal("capacity refusal mutated admission", admitted, err)
				}
				if a.task == nil {
					u := a.r.UsageSnapshot()
					if u.Input != 256 || u.Estimated || u.Reserved != reserved {
						t.Fatal("status did not use provider estimate", u)
					}
				}
			})
		}
	}
}

func newSummaryBudgetActor(t *testing.T, role string) *summaryBudgetActor {
	t.Helper()
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Continue the research.")
	a := &summaryBudgetActor{r: r}
	if role == "main" {
		a.turn = admissionTurn(t, r)
		return a
	}
	actor := "main/child_summary_budget"
	if role == "aside" {
		actor = "main/btw_summary_budget"
	}
	turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
	if err != nil {
		t.Fatal(err)
	}
	a.turn = turn
	a.task = &childTask{actor: actor, turn: turn, selection: r.selection, tools: r.Tools, aside: role == "aside", assignment: &childAssignment{accepting: true}}
	r.actorAssignments[actor] = a.task.assignment
	a.messages = []llm.Message{{Type: llm.TypeUserInput, Content: "Continue the isolated research.", InputTimeMS: 1000}}
	seedIsolatedHistory(t, r, actor, turn, a.messages)
	return a
}

func (a *summaryBudgetActor) selection() *llm.Selection {
	if a.task != nil {
		return &a.task.selection
	}
	return &a.r.selection
}
func (a *summaryBudgetActor) admit() (requestAdmission, error) {
	if a.task == nil {
		admitted, _, err := a.r.admitMain(context.Background(), a.turn, *a.selection())
		return admitted, err
	}
	admitted, next, err := a.r.admitChild(context.Background(), *a.task, a.messages, a.cursor)
	if err == nil {
		a.messages, a.cursor = admitted.Messages, next
	}
	return admitted, err
}
func (a *summaryBudgetActor) append(t *testing.T, message llm.Message) {
	t.Helper()
	actor, visible := "main", true
	if a.task != nil {
		actor, visible = a.task.actor, false
		a.messages = append(a.messages, message)
	}
	if _, err := a.r.Store.Append(a.r.Current(), a.turn, actor, "message", message.Role(), visible, message); err != nil {
		t.Fatal(err)
	}
}
func (a *summaryBudgetActor) snapshot(t *testing.T) llm.Request {
	t.Helper()
	actor, system, lineage, cursor := "main", systemTemplate, a.r.Current(), a.r.mainContext
	messages, err := a.r.Store.Messages(a.r.Current())
	if err != nil {
		t.Fatal(err)
	}
	if a.task == nil {
		messages = append(messages, pendingMainNotices(t, a.r)...)
		messages = append(messages, a.r.steeringMessagesLocked()...)
	} else {
		actor, system, lineage, cursor = a.task.actor, childSystemTemplate, a.task.actor, a.cursor
		if a.task.aside {
			system = asideSystemTemplate
		}
		messages = append([]llm.Message(nil), a.messages...)
		notices, _, err := a.r.childNoticeBatchLocked(a.task.assignment)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, notices...)
	}
	cm, _, err := a.r.runtimeContext(context.Background(), actor, *a.selection(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	if cm != nil {
		messages = append(messages, *cm)
	}
	messages, err = contextbuild.ProjectRetainedInputs(llm.ContextFor(*a.selection(), messages))
	if err != nil {
		t.Fatal(err)
	}
	return llm.Request{Selection: *a.selection(), System: system, Tools: a.r.Tools.Definitions(), Messages: messages, ConversationID: lineage, OutputTokens: a.selection().Model.Budget.OutputAllowance}
}
