package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
	"ttc/internal/historytest"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// A zero next-turn reserve is valid. The mandatory recovery warning must be
// counted explicitly, not restored after admitting a summary that only fits
// without it.
func TestRecoveryCompactionCountsPendingWarningBeforeFit(t *testing.T) {
	for _, actor := range []string{"main", "main/child_budget", "main/btw_budget"} {
		for _, fits := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fits=%v", actor, fits), func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				compactionBudget(t, r)
				seedRuntime(t, r, "Continue research")
				before := r.Current()
				aside := strings.Contains(actor, "/btw_")
				var turn string
				var err error
				if actor == "main" {
					turn, err = r.Store.BeginTurn(before, "user", r.selection)
				} else {
					turn, err = r.Store.BeginChildTurn(before, actor, r.selection)
				}
				if err != nil {
					t.Fatal(err)
				}
				request, err := r.Store.StartRequest(before, turn, actor, "coding", r.selection)
				if err != nil {
					t.Fatal(err)
				}
				warning := llm.Message{Type: llm.TypeRuntimeInstruction, RequestID: request, Content: prompts.Recovery}
				messages := []llm.Message{
					{Type: llm.TypeUserInput, Content: "Continue research", InputSource: "task", InputTimeMS: time.Now().UnixMilli()},
					{Type: llm.TypeAssistant, Content: strings.Repeat("Oversized partial evidence. ", 100)},
					warning,
				}
				if actor == "main" {
					if _, err := r.Store.Append(before, turn, actor, "message", "assistant", true, messages[1]); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := r.ensureRecovery(turn, actor, warning); err != nil {
					t.Fatal(err)
				}
				if actor == "main" {
					messages, err = r.Store.Messages(before)
					if err != nil {
						t.Fatal(err)
					}
				}
				retention, err := contextbuild.Retain(messages, 0, 700)
				if err != nil || retention.Start != len(messages) {
					t.Fatal("fixture did not summarize the pending warning", retention, err)
				}
				retained, err := contextbuild.RetainedInputMessages(messages, retention.Inputs, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				// Count the exact framed developer context used by the actual handoff.
				summary := "Partial evidence summarized; continue carefully."
				baseline := []llm.Message{llm.NewCompactionContext(summary)}
				baseline = append(baseline, retained...)
				system := childSystemTemplate
				if actor == "main" {
					system = systemTemplate
				}
				if aside {
					system = asideSystemTemplate
					baseline = append(baseline, llm.Message{Type: llm.TypeRuntimeInstruction, Content: btwInstruction})
				}
				runtime, _, err := r.runtimeContext(context.Background(), actor, r.selection, contextCursor{})
				if err != nil {
					t.Fatal(err)
				}
				input := append(append([]llm.Message(nil), baseline...), *runtime)
				u := estimateUsage(r.selection, system, r.Tools.Definitions(), input)
				warningTokens := contextbuild.Tokens([]llm.Message{warning})
				b := &r.selection.Model.Budget
				b.NextTurnInputReserve = 0
				b.ContextLimit = u.Input + u.Reserved + warningTokens/2
				if fits {
					b.ContextLimit += warningTokens
				}
				if err := b.Validate(); err != nil {
					t.Fatal("fixture budget is invalid", err)
				}
				if !compactionFits(r.selection, system, r.Tools.Definitions(), baseline, nil, runtime) {
					t.Fatal("baseline without warning does not fit")
				}
				withWarning, _ := contextbuild.AppendPendingMessage(baseline, &warning)
				withWarning = append(withWarning, *runtime)
				withWarningEstimate := contextbuild.EstimateInput(contextbuild.InputSnapshot{Selection: r.selection, System: system, Tools: r.Tools.Definitions(), Messages: withWarning})
				if codingInputFits(withWarningEstimate.Input, r.selection.Model.Budget, true) != fits {
					t.Fatal("fixture does not straddle the mandatory warning budget")
				}
				summaries := 0
				r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
					summaries++
					if !req.NoTools || !compactionMessagesContain(req.Messages, prompts.Recovery) {
						t.Fatal("summary must consume the original warning")
					}
					return emit(llm.StreamEvent{Kind: "text", Text: summary})
				}}
				var result []llm.Message
				if actor == "main" {
					_, err = r.compactContext(context.Background(), "", r.selection, &warning, false)
					if err == nil {
						result, err = r.Store.Messages(r.Current())
					}
				} else {
					result, _, err = r.compactChild(context.Background(), childTask{actor: actor, turn: turn, selection: r.selection, tools: r.Tools, aside: aside, estimator: &contextbuild.Estimator{}}, messages, contextCursor{}, &warning)
				}
				if summaries != 1 {
					t.Fatal("expected one summary", summaries, err)
				}
				if !fits {
					expected := prompts.SessionCompactionHeadroom
					if actor == "main" {
						expected = "/model"
					} else if aside {
						expected = "/btw"
					}
					if err == nil || !strings.Contains(err.Error(), expected) || r.Current() != before || result != nil || r.HasNotifications() {
						t.Fatal("handoff committed before fitting the required warning", err, r.Current(), result)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				found := 0
				for _, message := range result {
					if message.Content == warning.Content {
						found++
						if !reflect.DeepEqual(message, warning) {
							t.Fatal("warning provenance changed", message)
						}
					}
				}
				if found != 1 {
					t.Fatal("result omitted or duplicated pending warning", found)
				}
				entries, err := r.Store.Branch(r.Current(), 0)
				if err != nil {
					t.Fatal(err)
				}
				found = 0
				for _, entry := range entries {
					if entry.Actor != actor || entry.Kind != "message" || entry.Role != "developer" {
						continue
					}
					var message llm.Message
					if err := json.Unmarshal(entry.Content, &message); err != nil {
						t.Fatal(err)
					}
					if reflect.DeepEqual(message, warning) {
						found++
					}
				}
				if found != 1 {
					t.Fatal("active branch omitted or duplicated durable warning", found)
				}
			})
		}
	}
}

func TestChildRecoveryCompactionRestoresDurabilityAfterMainHandoff(t *testing.T) {
	for _, aside := range []bool{false, true} {
		t.Run(fmt.Sprint(aside), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			compactionBudget(t, r)
			seedCompactionHistory(t, r, strings.Repeat("Older main evidence. ", 400))
			before := r.Current()
			actor := "main/child_archived_warning"
			if aside {
				actor = "main/btw_archived_warning"
			}
			turn, err := r.Store.BeginChildTurn(before, actor, r.selection)
			if err != nil {
				t.Fatal(err)
			}
			request, err := r.Store.StartRequest(before, turn, actor, "coding", r.selection)
			if err != nil {
				t.Fatal(err)
			}
			warning := llm.Message{Type: llm.TypeRuntimeInstruction, RequestID: request, Content: prompts.Recovery}
			if _, err := r.ensureRecovery(turn, actor, warning); err != nil {
				t.Fatal(err)
			}
			messages := []llm.Message{
				{Type: llm.TypeUserInput, Content: "Continue isolated task", InputSource: "task", InputTimeMS: time.Now().UnixMilli()},
				{Type: llm.TypeAssistant, Content: strings.Repeat("Partial isolated evidence. ", 400)},
				warning,
			}
			summaries := 0
			r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				summaries++
				if strings.Contains(req.ConversationID, actor) {
					// Main archives the child warning while its isolated summary
					// is in flight; the durable entry is not in the new branch.
					if _, err := r.compactContext(ctx, "", r.selection, nil, false); err != nil {
						return err
					}
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Evidence summarized; continue the task."})
			}}
			result, _, err := r.compactChild(context.Background(), childTask{actor: actor, turn: turn, selection: r.selection, tools: r.Tools, aside: aside, estimator: &contextbuild.Estimator{}}, messages, contextCursor{}, &warning)
			if err != nil || summaries != 2 || r.Current() == before {
				t.Fatal("nested main/child handoff failed", err, summaries)
			}
			if _, added := contextbuild.AppendPendingMessage(result, &warning); added {
				t.Fatal("isolated replacement lost the warning")
			}
			if added, err := r.ensureRecovery(turn, actor, warning); err != nil || added {
				t.Fatal("durable warning was not restored exactly once", added, err)
			}
			main, err := r.Store.Messages(r.Current())
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range main {
				if message.Content == warning.Content {
					t.Fatal("child warning leaked into main context")
				}
			}
		})
	}
}

func TestPartialRecoveryCompactionZeroReserveContinuesAllActors(t *testing.T) {
	for _, actor := range []string{"main", "main/child_zero_reserve", "main/btw_zero_reserve"} {
		t.Run(actor, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			compactionBudget(t, r)
			r.selection.Model.Budget.NextTurnInputReserve = 0
			if err := r.selection.Model.Budget.Validate(); err != nil {
				t.Fatal(err)
			}
			partialRepeats := 250
			if strings.Contains(actor, "/btw_") {
				// The aside system is smaller; keep its partial output large
				// enough to trigger compaction while the structured prefix fits.
				partialRepeats = 350
			}
			coding, summaries := 0, 0
			var failedRequest int64
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if req.NoTools {
					summaries++
					return emit(llm.StreamEvent{Kind: "text", Text: "Partial evidence summarized; continue research."})
				}
				coding++
				wantSystem := systemTemplate
				if actor != "main" {
					wantSystem = childSystemTemplate
					if strings.Contains(actor, "/btw_") {
						wantSystem = asideSystemTemplate
					}
				}
				if req.System != wantSystem {
					t.Error("recovery/compaction changed role-specific system")
				}
				assertPersistedReceivedSystem(t, r, req)
				if coding == 1 {
					if err := historytest.DB(t, r.Store).QueryRow("SELECT max(id) FROM model_requests WHERE actor_id=? AND purpose='coding'", actor).Scan(&failedRequest); err != nil {
						return err
					}
					if err := partialRetryText(emit, strings.Repeat("Oversized partial research output. ", partialRepeats)); err != nil {
						return err
					}
					return partialRetryFailure(req)
				}
				warning := partialRetryWarning(t, req)
				if coding != 2 || summaries != 1 || req.PriorAttempts != 1 || warning.RequestID != failedRequest {
					t.Fatal("recovery lost pending warning or attempt count", coding, summaries, req.PriorAttempts, warning)
				}
				if !contextbuild.Fits(req.Selection, req.System, req.Tools, req.Messages, false) {
					t.Fatal("recovery bypassed request admission")
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Recovered after compaction"})
			}}
			var err error
			if actor == "main" {
				err = r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Recover this task"})
			} else {
				seedRuntime(t, r, "Private main context")
				turn, e := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
				if e != nil {
					t.Fatal(e)
				}
				err = r.runChild(context.Background(), childTask{actor: actor, turn: turn, prompt: "Isolated task", selection: r.selection, tools: r.Tools, aside: strings.Contains(actor, "/btw_"), estimator: &contextbuild.Estimator{}}, io.Discard, io.Discard)
				partialRetryNoMainLeak(t, r)
			}
			if err != nil || coding != 2 || summaries != 1 {
				t.Fatal("expected one compaction before recovery", err, coding, summaries)
			}
		})
	}
}
