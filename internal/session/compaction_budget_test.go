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
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// summaryBudgetActor exercises the production admission paths while retaining
// the isolated actor's worker-owned history and delivery cursor between calls.
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

func TestCompactionHandoffReservesSummaryAndNextTurnHeadroom(t *testing.T) {
	retained := []llm.Message{llm.NewCompactionContext("Summary handoff.")}
	notices := []llm.Message{{Type: llm.TypeRuntimeNotification, Content: `{"type":"job_exit","job_id":"pending"}`}}
	runtime := &llm.Message{Type: llm.TypeRuntimeContext, Content: "Current runtime state"}
	messages := append(append(append([]llm.Message(nil), retained...), notices...), *runtime)
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprintf("extra=%d", extra), func(t *testing.T) {
			selection := llm.Selection{Model: llm.ScriptModel()}
			b := &selection.Model.Budget
			input := contextbuild.EstimateInput(contextbuild.InputSnapshot{Selection: selection, Messages: messages}).Input
			b.ContextLimit = input + b.SummaryOutputAllowance + compactionInstructionTokens() + b.OutputAllowance + b.EstimationMargin + b.NextTurnInputReserve + extra
			if !contextbuild.Fits(selection, "", nil, messages, true) {
				t.Fatal("fixture must fit the coding handoff independently of the summary reserve")
			}
			if fits := compactionFits(selection, "", nil, retained, notices, runtime); fits != (extra > 0) {
				t.Fatalf("handoff at coding reserve boundary: fits=%v budget=%+v", fits, *b)
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
			guidance := llm.Message{Type: llm.TypeChildGuidance, Content: strings.Repeat("Pending parent guidance. ", 200)}
			assignment.queued = []llm.Message{guidance}
			assignment.queuedTokens = noticeTokens(guidance)
			source, err := a.r.Store.Append(a.r.Current(), "", a.task.actor, "status", "", false, map[string]any{"type": "runtime_event", "body": map[string]string{"type": "job_exit", "job_id": "pending_handoff"}})
			if err != nil {
				t.Fatal(err)
			}
			assignment.queueRuntimeNotice(source, "")
			notices, _, err := a.r.childNoticeBatchLocked(assignment)
			if err != nil || len(notices) != 2 {
				t.Fatal("fixture must contain both guidance and runtime notice", notices, err)
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
				input := contextbuild.EstimateInput(contextbuild.InputSnapshot{Selection: *a.selection(), System: system, Tools: a.r.Tools.Definitions(), Messages: messages}).Input
				b := a.selection().Model.Budget
				return input + b.OutputAllowance + b.EstimationMargin + b.NextTurnInputReserve + b.SummaryOutputAllowance + compactionInstructionTokens()
			}
			withoutNotices, withNotices := required(nil), required(notices)
			a.selection().Model.Budget.ContextLimit = withoutNotices + (withNotices-withoutNotices)/2
			if !compactionFits(*a.selection(), system, a.r.Tools.Definitions(), replacement, nil, cm) {
				t.Fatal("fixture must fit if pending delivery is incorrectly omitted")
			}
			if compactionFits(*a.selection(), system, a.r.Tools.Definitions(), replacement, notices, cm) {
				t.Fatal("fixture must refuse the handoff including pending delivery")
			}
			before := a.r.Current()
			cursor := a.cursor
			queuedTokens := assignment.queuedTokens
			// Keep summary inference below capacity so this regression reaches
			// the independent, uncalibrated replacement-handoff validation.
			prefix := contextbuild.InputSnapshot{Selection: *a.selection(), System: system, Tools: a.r.Tools.Definitions(), Messages: prepared.prefix, Lineage: a.task.actor, ReplayEstimator: a.r.Provider}
			a.estimator().ObserveRequest(1, a.estimator().Prepare(prefix))
			if _, ok := a.estimator().ObserveUsage(1, 256); !ok {
				t.Fatal("prefix calibration was not registered")
			}
			summaries := 0
			a.r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
				summaries++
				return emit(llm.StreamEvent{Kind: "text", Text: summary})
			}}
			result, next, err := a.r.compactChild(context.Background(), *a.task, a.messages, cursor, nil)
			var capacity *compactionCapacityError
			if !errors.As(err, &capacity) || !recoverableCompaction(err) || result != nil || summaries != 1 {
				t.Fatal("pending delivery should reject the handoff recoverably", result, next, err, summaries)
			}
			if a.r.Current() != before || !reflect.DeepEqual(cursor, next) || !reflect.DeepEqual(assignment.queued, []llm.Message{guidance}) || assignment.queuedTokens != queuedTokens || len(assignment.runtimeNotices) != 1 || assignment.runtimeNotices[0].seq != source {
				t.Fatal("failed handoff changed context/cursor or consumed pending delivery")
			}
			var successes, acknowledged int
			if err := historytest.DB(t, a.r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='actor_compacted' OR json_extract(content_json,'$.body.type')='child_compacted'`).Scan(&successes); err != nil || successes != 0 {
				t.Fatal("failed handoff published compaction success", successes, err)
			}
			if err := historytest.DB(t, a.r.Store).QueryRow(`SELECT count(*) FROM entries WHERE id=? AND delivered_request_id IS NOT NULL`, source).Scan(&acknowledged); err != nil || acknowledged != 0 {
				t.Fatal("failed handoff acknowledged runtime notice", acknowledged, err)
			}
		})
	}
}

func TestSummaryBudgetCompactsBeforeCodingConsumesFreshResults(t *testing.T) {
	a := newSummaryBudgetActor(t, "main")
	first, err := a.admit()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.estimator().ObserveUsage(first.RequestID, 256); !ok {
		t.Fatal("initial request did not register feedback")
	}
	if err := a.r.Store.FinishRequest(first.RequestID, "completed", nil); err != nil {
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
	b := &a.selection().Model.Budget
	b.RecentTokensMin, b.RecentTokensMax = 0, 100
	estimate := a.estimator().Estimate(snapshot)
	b.ContextLimit = estimate.Input + b.OutputAllowance + b.EstimationMargin + b.SummaryOutputAllowance + compactionInstructionTokens()
	snapshot.Selection = *a.selection()
	if !contextbuild.FitsInput(estimate.Input, *b, false) || codingInputFits(estimate.Input, *b, false) || estimate.Input >= estimate.LocalInput {
		t.Fatal("fixture must fit the old coding gate but need compaction with the new reserves", estimate, *b)
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
				t.Fatal("summary must precede consumption and exclude the entire fresh cycle")
			}
			if !reflect.DeepEqual(req.Messages[len(req.Messages)-1], compactionInstruction()) {
				t.Fatal("summary request must end with the compaction instruction")
			}
			return emit(llm.StreamEvent{Kind: "text", Text: "Older evidence summarized; inspect the fresh results next."})
		}
		coding++
		if summaries != 1 || a.r.Current() == before {
			t.Fatal("coding consumed fresh results before automatic summary-safe compaction")
		}
		var retained []llm.Message
		for _, message := range req.Messages {
			if len(message.Calls) > 0 || message.Type == llm.TypeToolResult {
				retained = append(retained, message)
			}
		}
		if !reflect.DeepEqual(retained, fresh) {
			t.Fatal("handoff changed, reordered, or omitted fresh calls/results")
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Fresh results consumed after compaction."})
	}}
	if err := a.r.Run(nil); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 || coding != 1 {
		t.Fatal("unexpected inference sequence", summaries, coding)
	}
}

func TestSummarizePreflightUsesCalibratedStructuredInputAndFullDirective(t *testing.T) {
	for _, role := range []string{"main", "child", "aside"} {
		for _, focus := range []string{"", "x", strings.Repeat("Prioritize the unresolved measurements. 雪 ", 80)} {
			for _, extra := range []int{-1, 0, 1} {
				t.Run(fmt.Sprintf("%s/focus=%d/extra=%d", role, len(focus), extra), func(t *testing.T) {
					a := newSummaryBudgetActor(t, role)
					selection := *a.selection()
					a.append(t, llm.Message{Type: llm.TypeAssistant, Content: "Read the original evidence.", Calls: []llm.ToolCall{{ID: "evidence", Name: "read", Arguments: json.RawMessage(`{"path":"evidence.pdf"}`)}}, State: &llm.ReplayState{Provider: selection.Provider, Model: selection.Model.RequestID(), Version: 1, Items: []json.RawMessage{json.RawMessage(`{"opaque":"original replay"}`)}}})
					a.append(t, llm.Message{Type: llm.TypeToolResult, CallID: "evidence", Content: "Original document.", Files: []llm.BinaryFile{{Path: "evidence.pdf", MIMEType: "application/pdf", Bytes: 12345}}})
					a.append(t, llm.Message{Type: llm.TypeAssistant, Content: strings.Repeat("Older structured research evidence. ", 2500)})
					snapshot := a.snapshot(t)
					// Bind feedback to the exact coding headers, tool definitions and
					// lineage. Only the optional focus and final directive change input.
					estimator := a.estimator()
					estimator.ObserveRequest(1, estimator.Prepare(snapshot))
					if _, ok := estimator.ObserveUsage(1, 256); !ok {
						t.Fatal("coding snapshot did not accept provider feedback")
					}
					projected, err := contextbuild.ProjectRetainedInputs(llm.ContextFor(snapshot.Selection, snapshot.Messages))
					if err != nil {
						t.Fatal(err)
					}
					var instructions []llm.Message
					if focus != "" {
						instructions = append(instructions, llm.Message{Type: llm.TypeRuntimeInstruction, Content: fmt.Sprintf(prompts.CompactionFocus, focus)})
					}
					instructions = append(instructions, llm.Message{Type: llm.TypeRuntimeInstruction, Content: prompts.Compaction})
					want := append(append([]llm.Message(nil), projected...), instructions...)
					full := snapshot
					full.Messages = want
					estimate := estimator.Estimate(full)
					if estimate.Input != 256+contextbuild.Tokens(instructions) || estimate.Input >= estimate.LocalInput {
						t.Fatalf("summary must inherit coding calibration plus both fully framed instruction messages: %+v", estimate)
					}
					b := &snapshot.Selection.Model.Budget
					b.OutputAllowance, b.SummaryOutputAllowance, b.EstimationMargin = 701, 1301, 307
					b.ContextLimit = estimate.Input + b.SummaryOutputAllowance + b.EstimationMargin + extra
					summaryBudget := *b
					summaryBudget.OutputAllowance = b.SummaryOutputAllowance
					if contextbuild.FitsInput(estimate.LocalInput, summaryBudget, false) {
						t.Fatal("local maximum must falsely refuse this calibrated summary")
					}
					if !contextbuild.FitsInput(estimator.Estimate(snapshot).Input, summaryBudget, false) {
						t.Fatal("fixture must fit if the appended instruction is omitted")
					}
					if focus != "" {
						emptyFocus := full
						emptyFocus.Messages = append(append([]llm.Message(nil), projected...), compactionInstruction())
						if !contextbuild.FitsInput(estimator.Estimate(emptyFocus).Input, summaryBudget, false) {
							t.Fatal("fixture must fit if only the additional focus is omitted")
						}
						withoutFocusFraming := estimate.Input - contextbuild.Tokens(instructions[:1]) + contextbuild.Estimate(instructions[0].Content)
						if !contextbuild.FitsInput(withoutFocusFraming, summaryBudget, false) {
							t.Fatal("fixture must fit if the separate focus message framing is incorrectly omitted")
						}
					}
					calls := 0
					a.r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
						calls++
						if req.System != snapshot.System || !reflect.DeepEqual(req.Tools, snapshot.Tools) || req.ConversationID != snapshot.Lineage || !reflect.DeepEqual(req.Selection, snapshot.Selection) {
							t.Fatal("summary changed the coding header, tools, selection or cache namespace")
						}
						if !req.NoTools || req.OutputTokens != b.SummaryOutputAllowance || !reflect.DeepEqual(req.Messages, want) {
							t.Fatal("summary must preserve structured replay/attachments and append its full directive")
						}
						return emit(llm.StreamEvent{Kind: "text", Text: "Structured evidence summarized."})
					}}
					actor := "main"
					if a.task != nil {
						actor = a.task.actor
					}
					text, err := a.r.summarize(context.Background(), actor, a.turn, snapshot, estimator, focus, false)
					if extra > 0 {
						if err != nil || text != "Structured evidence summarized." || calls != 1 {
							t.Fatal("calibrated summary should fit without a local maximum", text, calls, err)
						}
						return
					}
					var capacity *compactionCapacityError
					if !errors.As(err, &capacity) || !recoverableCompaction(err) || text != "" || calls != 0 {
						t.Fatal("full directive at capacity must fail before inference", text, calls, err)
					}
					var requests int
					if err := historytest.DB(t, a.r.Store).QueryRow(`SELECT count(*) FROM model_requests WHERE purpose='compaction'`).Scan(&requests); err != nil || requests != 0 {
						t.Fatal("preflight refusal created a durable summary request", requests, err)
					}
				})
			}
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
	a.task = &childTask{actor: actor, turn: turn, selection: r.selection, tools: r.Tools, aside: role == "aside", estimator: &contextbuild.Estimator{}, assignment: &childAssignment{accepting: true}}
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

func (a *summaryBudgetActor) estimator() *contextbuild.Estimator {
	if a.task != nil {
		return a.task.estimator
	}
	return &a.r.estimator
}

func (a *summaryBudgetActor) admit() (history.Admission, error) {
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

func (a *summaryBudgetActor) snapshot(t *testing.T) contextbuild.InputSnapshot {
	t.Helper()
	actor, system, lineage, cursor := "main", systemTemplate, a.r.Current(), a.r.mainContext
	messages, err := a.r.Store.Messages(a.r.Current())
	if err != nil {
		t.Fatal(err)
	}
	if a.task == nil {
		messages = append(messages, a.r.notifications...)
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
	if a.task == nil {
		messages, err = contextbuild.ProjectRetainedInputs(messages)
		if err != nil {
			t.Fatal(err)
		}
	}
	return contextbuild.InputSnapshot{Selection: *a.selection(), System: system, Tools: a.r.Tools.Definitions(), Messages: messages, Lineage: lineage, ReplayEstimator: a.r.Provider}
}

func TestCodingAdmissionUsesCalibratedInputWithInstructionAndSummaryReserve(t *testing.T) {
	for _, role := range []string{"main", "child", "aside"} {
		for _, extra := range []int{-1, 0, 1} {
			t.Run(fmt.Sprintf("%s/extra=%d", role, extra), func(t *testing.T) {
				a := newSummaryBudgetActor(t, role)
				first, err := a.admit()
				if err != nil {
					t.Fatal(err)
				}
				// Both coding and summary use this actor's calibrated estimate,
				// not max(local, measured) or a second flattened transcript.
				if _, ok := a.estimator().ObserveUsage(first.RequestID, 256); !ok {
					t.Fatal("initial admission did not register provider feedback")
				}
				if err := a.r.Store.FinishRequest(first.RequestID, "completed", nil); err != nil {
					t.Fatal(err)
				}
				a.append(t, llm.Message{Type: llm.TypeAssistant, Content: strings.Repeat("Canonical research evidence. ", 1000)})
				actor := "main"
				if a.task != nil {
					actor = a.task.actor
				}
				if a.task == nil {
					err = a.r.queueNotification(`{"type":"job_exit","job_id":"pending_summary_budget"}`)
				} else {
					var source int64
					source, err = a.r.Store.Append(a.r.Current(), "", actor, "status", "", false, map[string]any{"type": "runtime_event", "body": map[string]string{"type": "job_exit", "job_id": "pending_summary_budget"}})
					if err == nil {
						a.task.assignment.queueRuntimeNotice(source, "")
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				// A changed runtime capability must remain undelivered on refusal;
				// unlike model identity, this does not invalidate calibration.
				a.selection().Model.Images = true
				a.r.publishJob(jobs.Snapshot{ID: "summary_budget_job", Owner: actor, Kind: "shell", Status: "completed"})
				snapshot := a.snapshot(t)
				estimate := a.estimator().Estimate(snapshot)
				b := &a.selection().Model.Budget
				reserved := b.OutputAllowance + b.EstimationMargin + b.SummaryOutputAllowance + contextbuild.Tokens([]llm.Message{compactionInstruction()})
				b.ContextLimit = estimate.Input + reserved + extra
				snapshot.Selection = *a.selection()
				if estimate.Input >= estimate.LocalInput || !contextbuild.FitsInput(estimate.Input, *b, false) {
					t.Fatalf("fixture must fit the old calibrated coding gate: %+v, budget=%+v", estimate, *b)
				}
				if contextbuild.FitsInput(estimate.LocalInput, *b, false) {
					t.Fatal("fixture must reject an uncalibrated local maximum")
				}
				usage := usageFromEstimate(*a.selection(), estimate)
				if usage.Input != estimate.Input || usage.Reserved != reserved || usage.Limit != b.ContextLimit || usage.Estimated != estimate.Estimated {
					t.Fatalf("usage must expose calibrated input and all coding/summary reserves: %+v", usage)
				}
				before, err := a.r.Store.Session(a.r.Current())
				if err != nil {
					t.Fatal(err)
				}
				notices := append([]llm.Message(nil), a.r.notifications...)
				cursor := a.r.mainContext
				var runtimeNotices []*pendingRuntimeNotice
				if a.task != nil {
					cursor = a.cursor
					runtimeNotices = append(runtimeNotices, a.task.assignment.runtimeNotices...)
				}
				admitted, err := a.admit()
				if a.task == nil {
					if got := a.r.UsageSnapshot(); got.Input != estimate.Input || got.Reserved != reserved {
						t.Fatalf("main admission usage = %+v, want input=%d reserve=%d", got, estimate.Input, reserved)
					}
				}
				if extra > 0 {
					if err != nil || admitted.RequestID == 0 {
						t.Fatalf("calibrated request fits despite its larger local estimate: admission=%+v err=%v", admitted, err)
					}
					return
				}
				if !errors.Is(err, errNeedsCompaction) {
					t.Errorf("instruction and summary reserves at capacity must trigger compaction: %v", err)
				}
				if admitted.RequestID != 0 {
					t.Error("refused coding request was durably admitted", admitted.RequestID)
				}
				after, err := a.r.Store.Session(a.r.Current())
				if err != nil {
					t.Fatal(err)
				}
				if after.EntryTip != before.EntryTip || !reflect.DeepEqual(notices, a.r.notifications) {
					t.Error("summary-budget refusal consumed pending notices or changed history")
				}
				if a.task == nil {
					if !reflect.DeepEqual(cursor, a.r.mainContext) {
						t.Error("summary-budget refusal advanced main runtime cursor")
					}
				} else if !reflect.DeepEqual(cursor, a.cursor) || !reflect.DeepEqual(runtimeNotices, a.task.assignment.runtimeNotices) {
					t.Error("summary-budget refusal advanced isolated cursor or consumed runtime notices")
				}
				var acknowledged int
				if err := historytest.DB(t, a.r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='runtime_event' AND delivered_request_id IS NOT NULL`).Scan(&acknowledged); err != nil || acknowledged != 0 {
					t.Error("refused request acknowledged a pending source", acknowledged, err)
				}
			})
		}
	}
}
