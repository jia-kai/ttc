package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

func TestChildAdmissionUsesReportedInput(t *testing.T) {
	for _, aside := range []bool{false, true} {
		t.Run(map[bool]string{false: "child", true: "aside"}[aside], func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			compactionBudget(t, r)
			seedRuntime(t, r, "parent")
			actor := "main/child_estimator"
			turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
			if err != nil {
				t.Fatal(err)
			}
			task := childTask{actor: actor, turn: turn, selection: r.selection, tools: r.Tools, aside: aside, estimator: &contextbuild.Estimator{}}
			messages := []llm.Message{{Type: llm.TypeUserInput, Content: "Isolated task"}}
			admitted, _, err := r.admitChild(context.Background(), task, messages, contextCursor{})
			if err != nil {
				t.Fatal(err)
			}
			system := childSystemTemplate
			if aside {
				system = asideSystemTemplate
			}
			snapshot := contextbuild.InputSnapshot{Selection: task.selection, System: system, Tools: task.tools.Definitions(), Messages: admitted.Messages, Lineage: actor, ReplayEstimator: r.Provider}
			local := contextbuild.EstimateInput(snapshot).Input
			if _, ok := task.estimator.ObserveUsage(admitted.RequestID, local+1000); !ok {
				t.Fatal("admission did not register the request")
			}
			if err := r.Store.FinishRequest(admitted.RequestID, "completed", nil); err != nil {
				t.Fatal(err)
			}
			task.selection.Model.Budget.ContextLimit = local + task.selection.Model.Budget.OutputAllowance + task.selection.Model.Budget.EstimationMargin + task.selection.Model.Budget.SummaryOutputAllowance + compactionInstructionTokens() + 500
			if _, _, err := r.admitChild(context.Background(), task, messages, contextCursor{}); !errors.Is(err, errNeedsCompaction) {
				t.Fatal("reported input should require compaction", err)
			}
			// A fresh estimator still fits: rejection above must be calibrated,
			// not an unrelated change in the local request projection.
			task.estimator = &contextbuild.Estimator{}
			if _, _, err := r.admitChild(context.Background(), task, messages, contextCursor{}); err != nil {
				t.Fatal("uncalibrated request should fit", err)
			}
		})
	}
}

func TestChildCompactionResetsEstimatorOnlyOnSuccess(t *testing.T) {
	for _, aside := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(map[bool]string{false: "child", true: "aside"}[aside]+map[bool]string{false: "/success", true: "/failure"}[fail], func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				compactionBudget(t, r)
				seedRuntime(t, r, "parent")
				actor := "main/child_reset_estimator"
				turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
				if err != nil {
					t.Fatal(err)
				}
				task := childTask{actor: actor, turn: turn, selection: r.selection, tools: r.Tools, aside: aside, estimator: &contextbuild.Estimator{}}
				messages := []llm.Message{{Type: llm.TypeUserInput, Content: "Research task", InputTimeMS: 1000}, {Type: llm.TypeAssistant, Content: strings.Repeat("Earlier evidence. ", 500)}}
				seedIsolatedHistory(t, r, actor, turn, messages)
				snapshot := contextbuild.InputSnapshot{Selection: task.selection, System: childSystemTemplate, Tools: task.tools.Definitions(), Messages: messages, Lineage: actor, ReplayEstimator: r.Provider}
				observation := task.estimator.Prepare(snapshot)
				task.estimator.ObserveRequest(1, observation)
				task.estimator.ObserveUsage(1, observation.LocalInput+500)
				failure := errors.New("synthetic summary failure")
				r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
					if fail {
						return failure
					}
					if err := emit(llm.StreamEvent{Kind: "text", Text: "Earlier research complete."}); err != nil {
						return err
					}
					return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 12345}})
				}}
				_, _, err = r.compactChild(context.Background(), task, messages, contextCursor{}, nil)
				if fail && !errors.Is(err, failure) || !fail && err != nil {
					t.Fatal("unexpected compaction result", err)
				}
				got := task.estimator.Estimate(snapshot)
				want := observation.LocalInput
				if fail {
					want += 500
				}
				if got.Input != want || got.Estimated == fail {
					t.Fatalf("compaction estimator state: got %+v, want input=%d estimated=%v", got, want, !fail)
				}
			})
		}
	}
}

func TestPersistentChildUnusableFeedbackPreservesEstimator(t *testing.T) {
	for _, feedback := range []string{"missing", "negative", "failed_stream"} {
		t.Run(feedback, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			var latest contextbuild.InputSnapshot
			requests := 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				requests++
				latest = contextbuild.InputSnapshot{Selection: req.Selection, System: req.System, Tools: req.Tools, Messages: req.Messages, Lineage: req.ConversationID, ReplayEstimator: r.Provider}
				if err := emit(llm.StreamEvent{Kind: "text", Text: "Completed task"}); err != nil {
					return err
				}
				var usage *llm.Usage
				if requests == 1 {
					usage = &llm.Usage{InputTokens: contextbuild.EstimateInput(latest).Input + 500}
				} else if feedback == "negative" {
					usage = &llm.Usage{InputTokens: -1}
				} else if feedback == "failed_stream" {
					usage = &llm.Usage{InputTokens: contextbuild.EstimateInput(latest).Input + 900}
				}
				if err := emit(llm.StreamEvent{Kind: "completed", Usage: usage}); err != nil {
					return err
				}
				if requests == 2 && feedback == "failed_stream" {
					return errors.New("synthetic stream failure after usage")
				}
				return nil
			}}
			_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"first","label":"estimate"}`)}})
			first := childInvocation(t, r, ids[0], `{"persistent":true,"prompt":"first","label":"estimate"}`)
			if first["ok"] != true {
				t.Fatal(first)
			}
			childID := first["child_id"].(string)
			child := r.children[childID]
			estimator := child.estimator
			if estimator == nil {
				t.Fatal("production child did not retain its estimator")
			}
			args, _ := json.Marshal(map[string]any{"persistent": true, "prompt": "follow up", "child_id": childID})
			second := childInvocation(t, r, ids[0], string(args))
			wantStatus := "completed"
			if feedback == "failed_stream" {
				wantStatus = "failed"
			}
			if second["assignment_status"] != wantStatus || requests != 2 || child.estimator != estimator {
				t.Fatal("follow-up lost estimator ownership", second, requests)
			}
			got := estimator.Estimate(latest)
			if local := contextbuild.EstimateInput(latest).Input; got.Input != local+500 || !got.Estimated {
				t.Fatalf("unusable feedback replaced calibration: got %+v, local=%d", got, local)
			}
		})
	}
}
