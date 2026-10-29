package context

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"ttc/internal/llm"
)

func calibratedEstimator(t *testing.T, snapshot InputSnapshot, reported int) *Estimator {
	t.Helper()
	e := &Estimator{}
	e.ObserveRequest(1, e.Prepare(snapshot))
	if _, ok := e.ObserveUsage(1, reported); !ok {
		t.Fatal("failed to establish calibration")
	}
	return e
}

func TestEstimatorForkOverheadAndSharedInput(t *testing.T) {
	for _, role := range []struct {
		name, system string
		tools        []llm.ToolDefinition
	}{
		{name: "larger", system: strings.Repeat("aside instructions ", 20), tools: []llm.ToolDefinition{{Name: "aside", Description: "large schema", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)}}},
		{name: "smaller"},
		{name: "same", system: "instructions", tools: []llm.ToolDefinition{{Name: "parent", Description: "description", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	} {
		t.Run(role.name, func(t *testing.T) {
			source := estimatorSnapshot()
			source.Tools = []llm.ToolDefinition{{Name: "parent", Description: "description", Parameters: json.RawMessage(`{"type":"object"}`)}}
			replay := 30000
			source.Messages = append(source.Messages, llm.Message{Type: llm.TypeAssistant, State: &llm.ReplayState{Provider: "test", Model: "model", EstimatedTokens: &replay}})
			parent := calibratedEstimator(t, source, 500)
			anchorLocal := EstimateInput(source).LocalInput
			// The selected source may have progressed beyond the completed request.
			// Its pending observation is not itself a successful calibration.
			source.Messages = append(source.Messages, llm.Message{Type: llm.TypeUserInput, Content: "shared selected continuation"})
			parent.ObserveRequest(2, parent.Prepare(source))
			target := source
			target.System, target.Tools, target.Lineage = role.system, role.tools, "aside"
			child := parent.Fork(source, target.System, target.Tools, target.Lineage)
			if child == nil {
				t.Fatal("fork failed")
			}
			got := child.Estimate(target)
			want := 500 + EstimateInput(target).LocalInput - anchorLocal
			if !got.Estimated || got.Input != want || got.LocalInput != EstimateInput(target).LocalInput {
				t.Fatalf("fork estimate = %+v, want calibrated %d", got, want)
			}
			if got.Input >= got.LocalInput {
				t.Fatal("inflated replay lost its calibration", got)
			}
			if child.anchor == parent.anchor || child.pending != nil || parent.pending == nil || parent.pending.request != 2 {
				t.Fatal("fork shared state or changed parent pending")
			}
			// Normal compatibility is not relaxed by the explicit transfer.
			if got := parent.Estimate(target); got.Input != got.LocalInput || !got.Estimated {
				t.Fatal("ordinary parent compatibility weakened", got)
			}
			before := got
			target.Messages = append(target.Messages, llm.Message{Type: llm.TypeUserInput, Content: "actor-local continuation"})
			if got := child.Estimate(target); got.Input != before.Input+got.LocalInput-before.LocalInput || !got.Estimated {
				t.Fatal("actor-local delta lost calibration", got)
			}
		})
	}
}

func TestEstimatorForkIndependentLifecycle(t *testing.T) {
	source := estimatorSnapshot()
	parent := calibratedEstimator(t, source, 500)
	parent.ObserveRequest(2, parent.Prepare(source))
	target := source
	target.System, target.Lineage = "aside instructions", "aside"
	child := parent.Fork(source, target.System, nil, target.Lineage)
	if child == nil {
		t.Fatal("fork failed")
	}
	frozen := child.Estimate(target)
	if _, ok := child.ObserveUsage(2, 700); ok {
		t.Fatal("fork inherited pending request")
	}
	child.ObserveRequest(2, child.Prepare(target))
	if _, ok := parent.ObserveUsage(2, 900); !ok {
		t.Fatal("fork removed parent pending")
	}
	if got := child.Estimate(target); got != frozen {
		t.Fatal("later parent feedback changed child", got, frozen)
	}
	parent.Reset()
	if got := child.Estimate(target); got != frozen {
		t.Fatal("parent reset changed child", got, frozen)
	}
	if got, ok := child.ObserveUsage(2, 700); !ok || got.Estimated || got.Input != 700 {
		t.Fatal("independent child pending feedback failed", got, ok)
	}
	if got := child.Estimate(target); got.Estimated || got.Input != 700 {
		t.Fatal("child feedback did not supersede approximate anchor", got)
	}
	parent.ObserveRequest(3, parent.Prepare(source))
	parent.ObserveUsage(3, 800)
	child.ObserveRequest(3, child.Prepare(target))
	child.Reset()
	if _, ok := child.ObserveUsage(3, 1000); ok {
		t.Fatal("child reset retained pending")
	}
	if got := child.Estimate(target); got.Input != got.LocalInput || !got.Estimated {
		t.Fatal("child reset retained anchor", got)
	}
	if got := parent.Estimate(source); got.Estimated || got.Input != 800 {
		t.Fatal("child feedback/reset changed parent", got)
	}
}

func TestEstimatorForkRequiresCompatibleCompletedSource(t *testing.T) {
	mutations := map[string]func(*InputSnapshot){
		"provider":       func(s *InputSnapshot) { s.Selection.Provider += "x" },
		"model":          func(s *InputSnapshot) { s.Selection.Model.ID += "x" },
		"request model":  func(s *InputSnapshot) { s.Selection.Model.BaseID = "underlying" },
		"tier":           func(s *InputSnapshot) { s.Selection.Model.ServiceTier = "priority" },
		"variant":        func(s *InputSnapshot) { s.Selection.Variant += "x" },
		"system":         func(s *InputSnapshot) { s.System += "x" },
		"tools":          func(s *InputSnapshot) { s.Tools = []llm.ToolDefinition{{Name: "new"}} },
		"lineage":        func(s *InputSnapshot) { s.Lineage += "x" },
		"empty lineage":  func(s *InputSnapshot) { s.Lineage = "" },
		"tokenizer":      func(s *InputSnapshot) { s.TokenEstimator = sharedTokenizer{} },
		"invalid model":  func(s *InputSnapshot) { s.Selection.Model.ID = "" },
		"invalid type":   func(s *InputSnapshot) { s.Messages = []llm.Message{{Type: "invalid"}} },
		"invalid retain": func(s *InputSnapshot) { s.Messages[0].RetainedAtMS = 1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s := estimatorSnapshot()
			parent := calibratedEstimator(t, s, 500)
			mutate(&s)
			if child := parent.Fork(s, "aside", nil, "aside"); child != nil {
				t.Fatal("fork accepted incompatible/invalid source")
			}
		})
	}
	s := estimatorSnapshot()
	var parent Estimator
	if child := parent.Fork(s, "aside", nil, "aside"); child != nil {
		t.Fatal("fork accepted missing calibration")
	}
	parent.ObserveRequest(1, parent.Prepare(s))
	if child := parent.Fork(s, "aside", nil, "aside"); child != nil {
		t.Fatal("fork used pending observation")
	}
	parent.ObserveUsage(1, 500)
	if child := parent.Fork(s, "aside", nil, ""); child != nil {
		t.Fatal("fork accepted empty target lineage")
	}
	parent.Reset()
	if child := parent.Fork(s, "aside", nil, "aside"); child != nil {
		t.Fatal("fork survived reset")
	}
	if child := (*Estimator)(nil).Fork(s, "aside", nil, "aside"); child != nil {
		t.Fatal("nil receiver produced a fork")
	}
}

func TestEstimatorForkRejectsInvalidCounts(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, tc := range []struct {
		name     string
		reported int
		mutate   func(*Estimator, *InputSnapshot, *string)
	}{
		{name: "negative reported", reported: 500, mutate: func(e *Estimator, _ *InputSnapshot, _ *string) { e.anchor.reported = -1 }},
		{name: "negative baseline", reported: 500, mutate: func(e *Estimator, _ *InputSnapshot, _ *string) { e.anchor.local = -1 }},
		{name: "negative source", mutate: func(_ *Estimator, s *InputSnapshot, _ *string) { s.Messages = nil }},
		{name: "overflow source", reported: maxInt, mutate: func(_ *Estimator, s *InputSnapshot, _ *string) {
			s.Messages = append(s.Messages, llm.Message{Type: llm.TypeUserInput, Content: "more"})
		}},
		{name: "negative target", mutate: func(_ *Estimator, _ *InputSnapshot, system *string) { *system = "" }},
		{name: "overflow target", reported: maxInt, mutate: func(_ *Estimator, _ *InputSnapshot, system *string) { *system = strings.Repeat("longer", 30) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := estimatorSnapshot()
			parent := calibratedEstimator(t, s, tc.reported)
			system := s.System
			tc.mutate(parent, &s, &system)
			if child := parent.Fork(s, system, nil, "aside"); child != nil {
				t.Fatal("invalid calibrated counts produced a fork")
			}
		})
	}
	// Huge replay annotations exercise local overflow without allocating huge
	// payloads. Multiple overflowing additions must not wrap back to validity.
	for _, counts := range [][]int{{maxInt}, {maxInt, maxInt, 100}} {
		s := estimatorSnapshot()
		parent := calibratedEstimator(t, s, 500)
		s.Messages = nil
		for _, count := range counts {
			count := count
			s.Messages = append(s.Messages, llm.Message{Type: llm.TypeAssistant, State: &llm.ReplayState{Provider: "test", Model: "model", EstimatedTokens: &count}})
		}
		if got := EstimateInput(s); got.Input != -1 || got.LocalInput != -1 {
			t.Fatal("local overflow was not rejected", got)
		}
		if child := parent.Fork(s, "aside", nil, "aside"); child != nil {
			t.Fatal("overflowing local input produced a fork")
		}
	}
	// A valid source can overflow only after the target's role overhead is
	// added, even when the endpoint-reported occupancy is comfortably small.
	s := estimatorSnapshot()
	replay := maxInt - Estimate(s.System)
	s.Messages = []llm.Message{{Type: llm.TypeAssistant, State: &llm.ReplayState{Provider: "test", Model: "model", EstimatedTokens: &replay}}}
	parent := calibratedEstimator(t, s, 500)
	if child := parent.Fork(s, strings.Repeat("larger system", 10), nil, "aside"); child != nil {
		t.Fatal("overflowing target local count produced a fork")
	}
}

func TestEstimatorForkApproximateAndExactRestores(t *testing.T) {
	for _, approximate := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "approximate"}[approximate], func(t *testing.T) {
			s := estimatorSnapshot()
			var parent Estimator
			if approximate {
				if !parent.RestoreApproximate(s, 500) {
					t.Fatal("approximate restore failed")
				}
			} else if err := parent.Restore(*parent.Prepare(s).Checkpoint(), 500, s.Lineage); err != nil {
				t.Fatal(err)
			}
			child := parent.Fork(s, s.System, s.Tools, s.Lineage)
			if child == nil {
				t.Fatal("fork from restore failed")
			}
			if got := child.Estimate(s); got.Input != 500 || !got.Estimated {
				t.Fatal("identical fork must remain approximate", got)
			}
			if got := parent.Estimate(s); got.Input != 500 || got.Estimated != approximate {
				t.Fatal("fork changed parent exactness", got)
			}
		})
	}
}

func TestEstimatorForkProjectsReplayOnce(t *testing.T) {
	s := estimatorSnapshot()
	s.Messages = []llm.Message{{Type: llm.TypeAssistant, State: &llm.ReplayState{Provider: "test", Model: "model"}}}
	calls := 0
	s.TokenEstimator = replayEstimateFunc(func(llm.Message) int {
		calls++
		return 100 + calls
	})
	parent := calibratedEstimator(t, s, 500)
	calls = 0
	child := parent.Fork(s, s.System, s.Tools, "aside")
	if child == nil || calls != 1 || child.anchor.reported != 500 {
		t.Fatalf("fork did not freeze one shared replay projection: child=%+v, calls=%d", child, calls)
	}
	if s.Messages[0].State.EstimatedTokens != nil {
		t.Fatal("fork changed canonical replay")
	}
}

func TestEstimatorForkRejectsUnavailableCalibrationBeforeReplay(t *testing.T) {
	for _, reason := range []string{"missing", "pending", "lineage", "settings", "schemas"} {
		t.Run(reason, func(t *testing.T) {
			s := estimatorSnapshot()
			s.Messages = []llm.Message{{Type: llm.TypeAssistant, State: &llm.ReplayState{Provider: "test", Model: "model"}}}
			calls := 0
			s.TokenEstimator = replayEstimateFunc(func(llm.Message) int { calls++; return 100 })
			parent := &Estimator{}
			if reason != "missing" {
				parent.ObserveRequest(1, parent.Prepare(s))
				if reason != "pending" {
					parent.ObserveUsage(1, 500)
				}
			}
			calls = 0
			switch reason {
			case "lineage":
				s.Lineage = "different"
			case "settings":
				s.Selection.Variant = "different"
			case "schemas":
				s.Tools = []llm.ToolDefinition{{Name: "different"}}
			}
			if fork := parent.Fork(s, "aside", nil, "aside"); fork != nil || calls != 0 {
				t.Fatalf("unavailable calibration scanned replay: fork=%v calls=%d", fork, calls)
			}
		})
	}
}

func TestEstimatorForkCanonicalRetainedInputFromProjectedAnchor(t *testing.T) {
	source := estimatorSnapshot()
	source.Messages = []llm.Message{
		{Type: llm.TypeUserInput, Content: "Inspect the attachment", InputTimeMS: 1000, RetainedAtMS: 9000,
			Files: []llm.BinaryFile{{Path: "evidence.pdf", MIMEType: "application/pdf", Bytes: 4000}}},
		{Type: llm.TypeUserInput, Content: "Preserve units", InputSource: "steer", InputTimeMS: 2000, RetainedAtMS: 9000},
		{Type: llm.TypeAssistant, Content: strings.Repeat("Prior evidence. ", 1000)},
	}
	projected := source
	var err error
	projected.Messages, err = ProjectRetainedInputs(source.Messages)
	if err != nil {
		t.Fatal(err)
	}
	parent := calibratedEstimator(t, projected, 1000)
	target := source
	target.System, target.Tools, target.Lineage = "aside instructions", nil, "aside"
	child := parent.Fork(source, target.System, target.Tools, target.Lineage)
	if child == nil {
		t.Fatal("canonical retained input could not use projected main calibration")
	}
	got := child.Estimate(target)
	want := 1000 + EstimateInput(target).LocalInput - EstimateInput(projected).LocalInput
	if got.Input != want || !got.Estimated {
		t.Fatal("retained input or attachment counted differently during transfer", got, want)
	}
	if got := parent.Estimate(projected); got.Input != 1000 || got.Estimated {
		t.Fatal("fork changed projected parent anchor", got)
	}
}

func TestEstimatorCheckpointFingerprintsUnchanged(t *testing.T) {
	var e Estimator
	got := e.Prepare(estimatorSnapshot()).Checkpoint()
	want := Checkpoint{
		Version: 1, LocalInput: 22,
		Compatible: "2bce482a4a20f32b7d25b1dd7a5f5adb82875b6b50bcb3fb599181c2900a2f98",
		Exact:      "8aa961b7a2df7d302b10ea364e790e8ef513989aaba0d3db8e9f70eaec9f5632",
	}
	if got == nil || *got != want {
		t.Fatalf("checkpoint contract changed: got %+v, want %+v", got, want)
	}
}

func TestEstimatorForkConcurrent(t *testing.T) {
	s := estimatorSnapshot()
	parent := calibratedEstimator(t, s, 500)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if child := parent.Fork(s, "aside", nil, "aside"); child != nil {
					target := s
					target.System, target.Lineage = "aside", "aside"
					child.Estimate(target)
					child.ObserveRequest(1, child.Prepare(target))
					child.ObserveUsage(1, 700)
					child.Reset()
				}
				parent.ObserveRequest(int64(id+2), parent.Prepare(s))
				parent.ObserveUsage(int64(id+2), 500)
				if j%10 == 0 {
					parent.Reset()
				}
			}
		}(i)
	}
	wg.Wait()
}
