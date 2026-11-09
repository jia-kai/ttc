package session

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/historytest"
	"ttc/internal/llm"
)

func fixtureCheckpoint(version int) *llm.TokenCheckpoint {
	return &llm.TokenCheckpoint{Version: version, LocalInput: 100, Compatible: strings.Repeat("a", 64), Exact: strings.Repeat("b", 64)}
}

func TestLoadDelegatesSavedEvidenceAndEstimatesCurrentTarget(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		name := map[int]string{0: "missing", 1: "legacy-v1", 2: "provider-v2"}[version]
		t.Run(name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			var checkpoint *llm.TokenCheckpoint
			if version != 0 {
				checkpoint = fixtureCheckpoint(version)
			}
			p := &childProvider{
				estimate: func(context.Context, llm.Request) (llm.InputEstimate, error) {
					return llm.InputEstimate{Input: 100, Estimated: true, Checkpoint: checkpoint}, nil
				},
				stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
					if err := emit(llm.StreamEvent{Kind: "text", Text: "Saved answer."}); err != nil {
						return err
					}
					return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 777, OutputTokens: 5}})
				},
			}
			r.Provider = p
			if _, err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Save evidence."}); err != nil {
				t.Fatal(err)
			}
			before := r.UsageSnapshot()
			restored, estimated := 0, 0
			p.restore = func(got llm.TokenCheckpoint, usage llm.Usage) error {
				restored++
				if checkpoint == nil || got != *checkpoint || usage.InputTokens != 777 || usage.OutputTokens != 5 {
					t.Fatal("incorrect restore evidence", got, usage)
				}
				return nil
			}
			p.estimate = func(_ context.Context, req llm.Request) (llm.InputEstimate, error) {
				estimated++
				messages, err := r.Store.Messages(req.ConversationID)
				if err != nil {
					t.Fatal(err)
				}
				messages, err = contextbuild.ProjectRetainedInputs(llm.ContextFor(req.Selection, messages))
				if err != nil {
					t.Fatal(err)
				}
				if req.ConversationID == "" || req.System != systemTemplate || !reflect.DeepEqual(req.Tools, r.defaultTools) || !reflect.DeepEqual(req.Messages, messages) {
					t.Fatal("load estimated wrong target", req)
				}
				return llm.InputEstimate{Input: 321, Estimated: true}, nil
			}
			if err := r.Load(r.Current()); err != nil {
				t.Fatal(err)
			}
			wantRestored := 1
			if version == 0 {
				wantRestored = 0
			}
			if restored != wantRestored || estimated != 1 {
				t.Fatal("legacy data was reconstructed or restore skipped", restored, estimated)
			}
			got := r.UsageSnapshot()
			if got.Input != 321 || !got.Estimated || got.Reported == nil || got.Reported.Tokens.InputTokens != 777 || got.Totals.Requests != before.Totals.Requests {
				t.Fatal("load ignored provider estimate or response usage", got)
			}
		})
	}
}

func TestLoadEstimateFailurePreservesActiveRuntimeAndRestoredProviderEvidence(t *testing.T) {
	for _, stage := range []string{"restore", "estimate", "activation"} {
		t.Run(stage, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			p := &childProvider{
				estimate: func(context.Context, llm.Request) (llm.InputEstimate, error) {
					return llm.InputEstimate{Input: 100, Estimated: true, Checkpoint: fixtureCheckpoint(2)}, nil
				},
				stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
					return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 777}})
				},
			}
			r.Provider = p
			if _, err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Save evidence."}); err != nil {
				t.Fatal(err)
			}
			saved, generation, tools, before := r.Current(), r.Generation(), r.Tools, r.UsageSnapshot()
			failure := errors.New("estimation failure")
			restored := false
			p.restore = func(llm.TokenCheckpoint, llm.Usage) error {
				if stage == "restore" {
					return failure
				}
				restored = true
				return nil
			}
			p.estimate = func(context.Context, llm.Request) (llm.InputEstimate, error) {
				if stage == "estimate" {
					return llm.InputEstimate{Input: -1}, failure
				}
				return llm.InputEstimate{Input: 333, Estimated: true}, nil
			}
			if stage == "activation" {
				if _, err := historytest.DB(t, r.Store).Exec(`CREATE TRIGGER reject_load_activity BEFORE UPDATE OF last_activity_ms ON sessions BEGIN SELECT RAISE(ABORT,'activation blocked'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Load(saved); err == nil {
				t.Fatal("failed preparation succeeded")
			}
			if r.Current() != saved || r.Generation() != generation || r.Tools != tools || !reflect.DeepEqual(r.UsageSnapshot(), before) {
				t.Fatal("failed load replaced active state")
			}
			if restored != (stage != "restore") {
				t.Fatal("provider evidence was not retained independently", restored)
			}
		})
	}
}

func TestLoadInvalidProjectionPreservesActiveRuntime(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 1234}})
	}}
	if _, err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Save"}); err != nil {
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
	r.Provider = &childProvider{estimate: func(ctx context.Context, req llm.Request) (llm.InputEstimate, error) {
		estimate, err := llm.EstimateInput(ctx, req)
		estimate.Checkpoint = fixtureCheckpoint(2)
		return estimate, err
	}}
	seedRuntime(t, r, "Observe messages")
	turn := admissionTurn(t, r)
	if _, _, err := r.admitMain(context.Background(), turn, r.selection); err != nil {
		t.Fatal(err)
	}
	if err := r.queueNotification(`{"type":"image_click_cancelled"}`); err != nil {
		t.Fatal(err)
	}
	r.steers = []contextbuild.Input{{Text: "Also inspect units."}}
	cursor, notice := r.mainContext, pendingMainNotices(t, r)[0]
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
	if currentRequests != requests || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(r.mainContext, cursor) || len(pendingMainNotices(t, r)) != 1 || pendingMainNotices(t, r)[0].EventSeq != notice.EventSeq || len(r.steers) != 1 {
		t.Fatal("checkpoint failure partially admitted request")
	}
}

func TestUnusableFeedbackMatchesLiveAndReloadedAccounting(t *testing.T) {
	for _, invalid := range []llm.Usage{{InputTokens: -1}, {InputTokens: 12345, OutputTokens: -1}} {
		name := "negative-input"
		if invalid.OutputTokens < 0 {
			name = "negative-output"
		}
		t.Run(name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			calls := 0
			p := &childProvider{
				estimate: func(context.Context, llm.Request) (llm.InputEstimate, error) {
					return llm.InputEstimate{Input: 100, Estimated: true, Checkpoint: fixtureCheckpoint(2)}, nil
				},
				stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
					calls++
					usage := invalid
					if calls == 1 {
						usage = llm.Usage{InputTokens: 777, OutputTokens: 5}
					}
					if err := emit(llm.StreamEvent{Kind: "text", Text: "Done."}); err != nil {
						return err
					}
					return emit(llm.StreamEvent{Kind: "completed", Usage: &usage})
				},
			}
			r.Provider = p
			for _, text := range []string{"Measure.", "Continue."} {
				if _, err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: text}); err != nil {
					t.Fatal(err)
				}
			}
			if got := r.UsageSnapshot(); got.Reported != nil || got.Totals.Requests != 2 || got.Totals.ReportedRequests != 1 {
				t.Fatal("unusable feedback became live usage", got)
			}
			saved, err := r.Store.MainUsage(context.Background(), r.Current())
			if err != nil || saved.Reported != nil || saved.Anchor == nil || saved.Anchor.Tokens.InputTokens != 777 {
				t.Fatal("durable feedback differs from live", saved, err)
			}
			restores := 0
			p.restore = func(_ llm.TokenCheckpoint, usage llm.Usage) error {
				restores++
				if usage.InputTokens != 777 {
					t.Fatal("unusable feedback replaced durable evidence", usage)
				}
				return nil
			}
			if err := r.Load(r.Current()); err != nil {
				t.Fatal(err)
			}
			if restores != 1 || r.UsageSnapshot().Reported != nil {
				t.Fatal("reload restored different response accounting", restores, r.UsageSnapshot())
			}
		})
	}
}
