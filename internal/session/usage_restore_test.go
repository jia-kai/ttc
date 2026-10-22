package session

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/providers/openai"
	"ttc/internal/tool"
)

// tokenizerProvider keeps a controllable stream with the production tokenizer policy.
type tokenizerProvider struct{ Inference }

func (*tokenizerProvider) TokenizerID(model llm.ModelSpec) string {
	return (&openai.Adapter{}).TokenizerID(model)
}

func requestSnapshot(r *Runtime, req llm.Request) contextbuild.InputSnapshot {
	return contextbuild.InputSnapshot{Selection: req.Selection, System: req.System, Tools: req.Tools,
		Messages: req.Messages, Lineage: r.Current(), TokenEstimator: r.Provider}
}

func TestLoadRestoresMeasuredCalibrationForNextAdmission(t *testing.T) {
	for _, mode := range []string{"load", "restart", "legacy-load"} {
		t.Run(mode, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			var original contextbuild.InputSnapshot
			measured, originalLocal, calls := 0, 0, 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				calls++
				local := contextbuild.EstimateInput(requestSnapshot(r, req)).Input
				if calls == 1 {
					original = requestSnapshot(r, req)
					originalLocal = local
					measured = local + 1000
				} else {
					want := measured + local - originalLocal
					if got := r.UsageSnapshot(); got.Input != want || !got.Estimated || got.Input == local {
						t.Fatalf("admission lost measurement: %+v, want %d; local %d", got, want, local)
					}
				}
				if err := emit(llm.StreamEvent{Kind: "text", Text: "Saved answer."}); err != nil {
					return err
				}
				return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: measured, OutputTokens: 5}})
			}}
			if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Save this measurement."}); err != nil {
				t.Fatal(err)
			}
			saved := r.Current()
			if mode == "legacy-load" {
				if _, err := historytest.DB(t, r.Store).Exec("UPDATE request_observations SET checkpoint_json=NULL"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "restart" {
				prior := r
				r.Close()
				var err error
				r, err = New(context.Background(), prior.Store, prior.Workspace, prior.Provider, prior.CurrentSelection(), saved, prior.Skills, tool.WebSearchConfig{}, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(r.Close)
				r.AutoName = false
			} else {
				if _, err := r.Command("/load " + saved); err != nil {
					t.Fatal(err)
				}
			}
			got := r.UsageSnapshot()
			if got.Reported == nil || got.Reported.Tokens.InputTokens != measured {
				t.Fatal("missing measured response", got)
			}
			messages, err := r.Store.Messages(r.Current())
			if err != nil {
				t.Fatal(err)
			}
			current := contextbuild.InputSnapshot{Selection: r.CurrentSelection(), System: systemTemplate,
				Tools: r.Tools.Definitions(), Messages: messages, Lineage: r.Current(), TokenEstimator: r.Provider}
			local := contextbuild.EstimateInput(current).Input
			if got.Input != measured+local-originalLocal || got.Input == local || !got.Estimated {
				t.Fatalf("load lost calibration: %+v, original local=%d, new local=%d", got, originalLocal, local)
			}
			if mode == "restart" && got.Totals.Requests != 0 {
				t.Fatal("restart restored process totals", got.Totals)
			}
			if mode != "restart" && got.Totals.Requests != 1 {
				t.Fatal("load changed process totals", got.Totals)
			}
			// Exact identity remains exact only for a durable checkpoint. Legacy
			// reconstruction cannot establish which tool schemas originally ran.
			original.Lineage = r.Current()
			check := r.estimator.Estimate(original)
			if check.Input != measured || check.Estimated != (mode == "legacy-load") {
				t.Fatal("restored anchor identity", check)
			}
			if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Continue."}); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatal("load performed extra inference", calls)
			}
		})
	}
}

func TestSameVersionModelSwitchPreservesMeasuredCalibration(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	r.selection.Provider = "openai"
	r.selection.Model.ID = "gpt-6-sol"
	var baseline contextbuild.InputSnapshot
	measured, originalLocal, calls := 0, 0, 0
	r.Provider = &tokenizerProvider{Inference: &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		calls++
		snapshot := requestSnapshot(r, req)
		local := contextbuild.EstimateInput(snapshot).Input
		if calls == 1 {
			baseline, originalLocal, measured = snapshot, local, local+1000
		} else if calls == 2 {
			if got := r.UsageSnapshot(); got.Input != measured+local-originalLocal || !got.Estimated || got.Input == local {
				t.Fatal("model variant lost measurement", got, local)
			}
			// Native replay is still model-specific, even when tokenizers match.
			for _, m := range req.Messages {
				if m.State != nil {
					t.Fatal("variant switch reused foreign native replay")
				}
			}
		} else {
			if got := r.UsageSnapshot(); got.Input != local || !got.Estimated {
				t.Fatal("different model version reused calibration", got, local)
			}
		}
		if calls == 1 {
			if err := emit(llm.StreamEvent{Kind: "state", StateVersion: 1, StateItem: json.RawMessage(`{"type":"reasoning","encrypted_content":"old model"}`)}); err != nil {
				return err
			}
		}
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Done."}); err != nil {
			return err
		}
		var usage *llm.Usage
		if calls == 1 {
			usage = &llm.Usage{InputTokens: measured}
		}
		return emit(llm.StreamEvent{Kind: "completed", Usage: usage})
	}}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Measure."}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"gpt-6-luna", "gpt-6.1-sol"} {
		next := r.CurrentSelection()
		next.Model.ID = model
		if err := r.RequestModel(next); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ApplyModel(""); err != nil {
			t.Fatal(err)
		}
		if model == "gpt-6-luna" {
			baseline.Selection = next
			if got := r.estimator.Estimate(baseline); got.Input != measured || got.Estimated {
				t.Fatal("switch discarded tokenizer-compatible anchor", got)
			}
		}
		if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Continue."}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadInvalidCheckpointPreservesActiveRuntime(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 1234}})
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Save"}); err != nil {
		t.Fatal(err)
	}
	saved, generation, tools, before := r.Current(), r.Generation(), r.Tools, r.UsageSnapshot()
	if _, err := historytest.DB(t, r.Store).Exec("UPDATE request_observations SET checkpoint_json=json_set(checkpoint_json,'$.version',999)"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Command("/load " + saved); err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatal("load accepted invalid checkpoint", err)
	}
	if r.Current() != saved || r.Generation() != generation || r.Tools != tools || !reflect.DeepEqual(r.UsageSnapshot(), before) {
		t.Fatal("failed load destroyed active runtime")
	}
}

func TestLoadInvalidProjectionPreservesActiveRuntime(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 1234}})
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Save"}); err != nil {
		t.Fatal(err)
	}
	saved, generation, tools, before := r.Current(), r.Generation(), r.Tools, r.UsageSnapshot()
	if _, err := historytest.DB(t, r.Store).Exec(`UPDATE entries SET content_json=json_set(content_json,'$.input_source','invalid') WHERE role='user' AND json_extract(content_json,'$.type')='user_input'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Command("/load " + saved); err == nil || !strings.Contains(err.Error(), "invalid source") {
		t.Fatal("load accepted invalid projection", err)
	}
	if r.Current() != saved || r.Generation() != generation || r.Tools != tools || !reflect.DeepEqual(r.UsageSnapshot(), before) {
		t.Fatal("failed load destroyed active runtime")
	}
}

func TestCheckpointPersistenceFailureRollsBackAdmission(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe messages")
	turn := admissionTurn(t, r)
	if _, _, err := r.admitMain(context.Background(), turn, r.selection); err != nil {
		t.Fatal(err)
	}
	if err := r.queueNotification(`{"type":"image_click_cancelled"}`); err != nil {
		t.Fatal(err)
	}
	r.steers = []contextbuild.Input{{Text: "Also inspect units."}}
	cursor, notice := r.mainContext, r.notifications[0]
	before, err := r.Store.Branch(r.Current(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var requests int
	db := historytest.DB(t, r.Store)
	if err := db.QueryRow("SELECT count(*) FROM model_requests").Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_checkpoint BEFORE UPDATE OF input_json ON model_requests WHEN json_type(NEW.input_json,'$.input_estimate')='object' BEGIN SELECT RAISE(ABORT,'forced checkpoint failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.admitMain(context.Background(), turn, r.selection); err == nil || !strings.Contains(err.Error(), "checkpoint failure") {
		t.Fatal("checkpoint failure did not reject admission", err)
	}
	after, err := r.Store.Branch(r.Current(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var currentRequests int
	if err := db.QueryRow("SELECT count(*) FROM model_requests").Scan(&currentRequests); err != nil {
		t.Fatal(err)
	}
	if currentRequests != requests || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(r.mainContext, cursor) || len(r.notifications) != 1 || r.notifications[0].EventSeq != notice.EventSeq || len(r.steers) != 1 {
		t.Fatal("checkpoint failure partially admitted request")
	}
}

func TestUnusableFeedbackMatchesLiveAndReloadedAccounting(t *testing.T) {
	for _, invalid := range []llm.Usage{{InputTokens: -1}, {InputTokens: 12345, OutputTokens: -1}} {
		name := "negative input"
		if invalid.OutputTokens < 0 {
			name = "negative output"
		}
		t.Run(name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			var original contextbuild.InputSnapshot
			measured, calls := 0, 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				calls++
				usage := invalid
				if calls == 1 {
					original = requestSnapshot(r, req)
					measured = contextbuild.EstimateInput(original).Input + 1000
					usage = llm.Usage{InputTokens: measured, OutputTokens: 5}
				}
				if err := emit(llm.StreamEvent{Kind: "text", Text: "Done."}); err != nil {
					return err
				}
				return emit(llm.StreamEvent{Kind: "completed", Usage: &usage})
			}}
			for _, prompt := range []string{"Measure.", "Continue."} {
				if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: prompt}); err != nil {
					t.Fatal(err)
				}
			}
			if got := r.UsageSnapshot(); got.Reported != nil || got.Totals.Requests != 2 || got.Totals.ReportedRequests != 1 {
				t.Fatal("unusable feedback became live usage", got)
			}
			if got := r.estimator.Estimate(original); got.Input != measured || got.Estimated {
				t.Fatal("unusable feedback replaced live anchor", got)
			}
			saved, err := r.Store.MainUsage(context.Background(), r.Current())
			if err != nil || saved.Reported != nil || saved.Anchor == nil || saved.Anchor.Tokens.InputTokens != measured {
				t.Fatal("durable feedback differs from live", saved, err)
			}
			if _, err := r.Command("/load " + r.Current()); err != nil {
				t.Fatal(err)
			}
			original.Lineage = r.Current()
			if got := r.estimator.Estimate(original); got.Input != measured || got.Estimated {
				t.Fatal("reload restored different anchor", got)
			}
			if r.UsageSnapshot().Reported != nil {
				t.Fatal("reload restored unusable reported usage")
			}
		})
	}
}
