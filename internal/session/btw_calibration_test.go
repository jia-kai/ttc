package session

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

const btwCalibrationMeasured = 20000

// The initial main request fits locally; subsequent requests can fit only by
// retaining its endpoint calibration. The large output is compactable history,
// not an oversized user instruction that retention must preserve verbatim.
func btwCalibrationRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	b := &r.selection.Model.Budget
	b.ContextLimit = 1000000
	b.OutputAllowance, b.SummaryOutputAllowance, b.EstimationMargin = 256, 512, 64
	b.RecentTokensMin, b.RecentTokensMax, b.NextTurnInputReserve = 0, 700, 128
	seedCompactionHistory(t, r, strings.Repeat("Inherited research evidence remains frozen. ", 20000))
	return r
}

func TestBTWCalibrationAdmitsOversizedInheritedPrefix(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "idle"
		if active {
			name = "active"
		}
		t.Run(name, func(t *testing.T) {
			r := btwCalibrationRuntime(t)
			var original contextbuild.InputSnapshot
			var mainCalls, asideCalls, summaries atomic.Int32
			mainReady := make(chan llm.Request, 1)
			asideRequests := make(chan llm.Request, 2)
			r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if req.NoTools {
					summaries.Add(1)
					return fmt.Errorf("unexpected compaction before calibrated aside admission")
				}
				if strings.Contains(req.ConversationID, "/btw_") {
					asideRequests <- req
					asideCalls.Add(1)
					if err := emit(llm.StreamEvent{Kind: "text", Text: "Independent aside answer."}); err != nil {
						return err
					}
					// Deliberately unlike the main feedback: a later aside must not
					// inherit this measurement through a shared estimator.
					return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 1234, OutputTokens: 5}})
				}
				if mainCalls.Add(1) > 1 {
					mainReady <- req
					<-ctx.Done()
					return ctx.Err()
				}
				original = requestSnapshot(r, req)
				if err := emit(llm.StreamEvent{Kind: "text", Text: "Main request completed."}); err != nil {
					return err
				}
				return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: btwCalibrationMeasured, OutputTokens: 5}})
			}}
			if err := r.Run(nil); err != nil {
				t.Fatal(err)
			}
			r.selection.Model.Budget.ContextLimit = 40000
			var frozen []llm.Message
			if active {
				done := make(chan error, 1)
				go func() { done <- r.Run(nil) }()
				defer func() {
					r.Interrupt()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("main request did not stop")
					}
				}()
				select {
				case req := <-mainReady:
					frozen = req.Messages
				case <-time.After(5 * time.Second):
					t.Fatal("calibrated main request did not start")
				}
				// This committed tail is newer than the active request's frozen
				// prefix and must not silently become the aside's source.
				if _, err := r.Store.Append(r.Current(), "", "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "newer-than-frozen-prefix"}); err != nil {
					t.Fatal(err)
				}
			} else {
				var err error
				frozen, err = r.Store.Messages(r.Current())
				if err != nil {
					t.Fatal(err)
				}
				frozen = llm.ContextFor(r.selection, frozen)
			}
			before, err := r.Store.Messages(r.Current())
			if err != nil {
				t.Fatal(err)
			}
			sessionID := r.Current()
			for range 2 {
				id, err := r.StartBTW("Explain the inherited evidence.")
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				job, err := r.Jobs.Wait(ctx, "main", id, nil)
				cancel()
				if err != nil || job.Status != "completed" || !strings.Contains(job.Stdout, "Independent aside answer.") {
					t.Fatalf("calibrated aside failed before answering: %+v, %v", job, err)
				}
				req := <-asideRequests
				inherited, err := r.projectInheritedSkillCatalogs(frozen, req.ConversationID)
				if err != nil {
					t.Fatal(err)
				}
				if len(req.Messages) < len(inherited)+3 || !reflect.DeepEqual(req.Messages[:len(inherited)], inherited) {
					t.Fatal("aside changed or replaced the frozen inherited prefix")
				}
				if req.Messages[len(inherited)].Content != btwInstruction || req.System != asideSystemTemplate || !reflect.DeepEqual(req.Tools, r.Tools.Filter(readOnlyTool).Definitions()) {
					t.Fatal("aside did not use its own instruction, system and read-only tools")
				}
				local := contextbuild.EstimateInput(contextbuild.InputSnapshot{Selection: req.Selection, System: req.System, Tools: req.Tools, Messages: inherited, TokenEstimator: r.Provider})
				if local.Input <= req.Selection.Model.Budget.ContextLimit {
					t.Fatalf("fixture does not require inherited calibration: %+v", local)
				}
				if got := r.estimator.Estimate(original); got.Input != btwCalibrationMeasured || got.Estimated {
					t.Fatalf("aside feedback replaced main's exact anchor: %+v", got)
				}
			}
			after, err := r.Store.Messages(r.Current())
			if err != nil || !reflect.DeepEqual(before, after) || r.Current() != sessionID {
				t.Fatal("aside changed main history or continuation", err)
			}
			if active {
				r.mu.Lock()
				unchanged := reflect.DeepEqual(r.mainPrefix, frozen)
				r.mu.Unlock()
				if !unchanged {
					t.Fatal("aside changed the main request's frozen prefix")
				}
			}
			if asideCalls.Load() != 2 || summaries.Load() != 0 {
				t.Fatal("aside compacted or failed to reach the provider", asideCalls.Load(), summaries.Load())
			}
		})
	}
}

func TestBTWCalibrationFallbackIsConservative(t *testing.T) {
	for _, feedback := range []string{"missing", "incompatible"} {
		t.Run(feedback, func(t *testing.T) {
			r := btwCalibrationRuntime(t)
			var calls atomic.Int32
			r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
				if calls.Add(1) > 1 {
					return fmt.Errorf("uncalibrated oversized aside reached provider")
				}
				var usage *llm.Usage
				if feedback == "incompatible" {
					usage = &llm.Usage{InputTokens: btwCalibrationMeasured, OutputTokens: 5}
				}
				return emit(llm.StreamEvent{Kind: "completed", Usage: usage})
			}}
			if err := r.Run(nil); err != nil {
				t.Fatal(err)
			}
			r.selection.Model.Budget.ContextLimit = 40000
			if feedback == "incompatible" {
				r.selection.Variant = "different-settings"
			}
			before, err := r.Store.Messages(r.Current())
			if err != nil {
				t.Fatal(err)
			}
			id, err := r.StartBTW("Explain the inherited evidence.")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			job, err := r.Jobs.Wait(ctx, "main", id, nil)
			if err != nil || job.Status != "failed" || calls.Load() != 1 || !strings.Contains(job.Stderr, "compaction input exceeds model context") {
				t.Fatalf("missing/incompatible calibration must not bypass capacity checks: %+v, calls=%d, err=%v", job, calls.Load(), err)
			}
			after, err := r.Store.Messages(r.Current())
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("failed aside changed main history", err)
			}
		})
	}
}

func TestBTWCalibrationForkSupportsCompactionPreflight(t *testing.T) {
	r := btwCalibrationRuntime(t)
	var original contextbuild.InputSnapshot
	var summaries int
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if req.NoTools {
			summaries++
			if req.System != asideSystemTemplate || !compactionMessagesContain(req.Messages, "Inherited research evidence remains frozen.") {
				return fmt.Errorf("aside summary lost inherited evidence or headers")
			}
			return emit(llm.StreamEvent{Kind: "text", Text: "Inherited research summarized for the aside."})
		}
		original = requestSnapshot(r, req)
		return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: btwCalibrationMeasured, OutputTokens: 5}})
	}}
	if err := r.Run(nil); err != nil {
		t.Fatal(err)
	}
	r.selection.Model.Budget.ContextLimit = 40000
	prefix, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	actor := "main/btw_calibrated_compaction"
	tools := r.Tools.Filter(readOnlyTool)
	source := contextbuild.InputSnapshot{Selection: r.selection, System: systemTemplate, Tools: r.Tools.Definitions(), Messages: prefix, Lineage: r.Current(), TokenEstimator: r.Provider}
	estimator := r.estimator.Fork(source, asideSystemTemplate, tools.Definitions(), actor)
	if estimator == nil {
		t.Fatal("compatible measured main context did not seed an aside estimator")
	}
	turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
	if err != nil {
		t.Fatal(err)
	}
	messages := append(append([]llm.Message(nil), prefix...), llm.Message{Type: llm.TypeRuntimeInstruction, Content: btwInstruction}, llm.Message{Type: llm.TypeUserInput, Content: "Explain the inherited evidence.", InputSource: "btw", InputTimeMS: 1000})
	seedIsolatedHistory(t, r, actor, turn, messages[len(prefix):])
	snapshot := contextbuild.InputSnapshot{Selection: r.selection, System: asideSystemTemplate, Tools: tools.Definitions(), Messages: messages, Lineage: actor, TokenEstimator: r.Provider}
	local := contextbuild.EstimateInput(snapshot)
	if local.Input <= r.selection.Model.Budget.ContextLimit {
		t.Fatal("fixture must exceed local compaction preflight capacity", local)
	}
	if seeded := estimator.Estimate(snapshot); seeded.Input >= seeded.LocalInput || !seeded.Estimated {
		t.Fatal("aside fork must inherit approximate calibration", seeded)
	}
	compacted, _, err := r.compactChild(context.Background(), childTask{actor: actor, turn: turn, selection: r.selection, tools: tools, aside: true, estimator: estimator}, messages, contextCursor{}, nil)
	if err != nil || summaries != 1 || len(compacted) == 0 || !reflect.DeepEqual(compacted[0], llm.NewCompactionContext("Inherited research summarized for the aside.")) {
		t.Fatal("seeded aside calibration did not reach compaction provider", summaries, compacted, err)
	}
	if got := estimator.Estimate(snapshot); got.Input != got.LocalInput || !got.Estimated {
		t.Fatal("successful compaction did not reset the isolated estimator", got)
	}
	if got := r.estimator.Estimate(original); got.Input != btwCalibrationMeasured || got.Estimated {
		t.Fatal("aside compaction reset or replaced the main anchor", got)
	}
}
