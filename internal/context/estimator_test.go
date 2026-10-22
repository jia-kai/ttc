package context

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"ttc/internal/llm"
)

func TestFingerprintAllocationsDoNotScaleWithFields(t *testing.T) {
	payload := []byte("raw payload")
	allocations := func(fields int) float64 {
		return testing.AllocsPerRun(100, func() {
			writer := fingerprintWriter{Hash: sha256.New()}
			for i := 0; i < fields; i++ {
				writer.string("small field")
				writer.string("")
				writer.integer(int64(i))
				writer.bytes(payload)
			}
			var sum [sha256.Size]byte
			writer.Sum(sum[:0])
		})
	}
	one, many := allocations(1), allocations(100)
	if many > one {
		t.Fatalf("allocations scale with fields: one=%g, hundred=%g", one, many)
	}
	t.Logf("allocations per fingerprint: one field=%g, hundred fields=%g", one, many)
}

func estimatorSnapshot() InputSnapshot {
	return InputSnapshot{Selection: llm.Selection{Provider: "test", Model: llm.ModelSpec{ID: "model", Budget: llm.Budget{ContextLimit: 10000, OutputAllowance: 100, EstimationMargin: 20}}, Variant: "fast"}, System: "instructions", Lineage: "first", Messages: []llm.Message{{Type: llm.TypeUserInput, Content: "hello"}}}
}

func TestEstimateInputCategories(t *testing.T) {
	s := estimatorSnapshot()
	s.Tools = []llm.ToolDefinition{{Name: "tool", Description: "description", Parameters: json.RawMessage(`{"type":"object"}`)}}
	replay := 73
	s.Messages = append(s.Messages,
		llm.Message{Type: llm.TypeAssistant, Content: "call", Calls: []llm.ToolCall{{Name: "tool", Arguments: json.RawMessage(`{}`)}}},
		llm.Message{Type: llm.TypeToolResult, Content: "result", Files: []llm.BinaryFile{{MIMEType: "image/png"}, {MIMEType: "application/pdf", Bytes: 8000}}},
		llm.Message{Type: llm.TypeAssistant, Content: "ignored", State: &llm.ReplayState{Provider: "test", Model: "model", EstimatedTokens: &replay}},
		llm.Message{Type: llm.TypeRuntimeNotification, Content: "notice"})
	r := EstimateInput(s)
	content, _ := s.Messages[4].ModelContent()
	want := Estimate(s.System) + 16 + Estimate("description") + Estimate(string(s.Tools[0].Parameters)) + 16 + Estimate("hello") + 16 + Estimate("call") + 16 + Estimate("tool") + Estimate(`{}`) + 16 + Estimate(content) + 16 + Estimate("result") + 73 + 12096
	if !r.Estimated || r.Input != r.LocalInput || r.Input != want {
		t.Fatalf("result = %+v, want input %d", r, want)
	}
	if r.Input != Estimate(s.System)+16+Estimate("description")+Estimate(string(s.Tools[0].Parameters))+Tokens(s.Messages) {
		t.Fatal("local estimation disagrees with Tokens")
	}
}

func TestEstimatorFeedbackAndMissingFeedback(t *testing.T) {
	var e Estimator
	s := estimatorSnapshot()
	local := e.ObserveRequest(1, e.Prepare(s))
	for _, pair := range [][2]int{{2, 400}, {1, -1}} {
		if _, ok := e.ObserveUsage(int64(pair[0]), pair[1]); ok {
			t.Fatal("invalid feedback accepted")
		}
	}
	r, ok := e.ObserveUsage(1, 400)
	if !ok || r.Estimated || r.Input != 400 || r.LocalInput != local.Input {
		t.Fatalf("feedback = %+v, %v", r, ok)
	}
	if exact := e.Estimate(s); exact.Estimated || exact.Input != 400 {
		t.Fatalf("exact = %+v", exact)
	}
	if _, ok := e.ObserveUsage(1, 500); ok {
		t.Fatal("duplicate feedback accepted")
	}
	s.Messages = append(s.Messages, llm.Message{Type: llm.TypeAssistant, Content: "more"})
	changed := e.ObserveRequest(2, e.Prepare(s))
	if !changed.Estimated || changed.Input != 400+changed.LocalInput-local.LocalInput {
		t.Fatalf("calibrated = %+v", changed)
	}
	s.Messages = append(s.Messages, llm.Message{Type: llm.TypeUserInput, Content: "again"})
	latest := e.ObserveRequest(3, e.Prepare(s))
	if latest.Input != 400+latest.LocalInput-local.LocalInput {
		t.Fatalf("missing feedback lost anchor: %+v", latest)
	}
	if _, ok := e.ObserveUsage(2, 600); ok {
		t.Fatal("superseded feedback accepted")
	}
	e.Reset()
	if _, ok := e.ObserveUsage(3, 700); ok {
		t.Fatal("reset retained pending")
	}
	if got := e.Estimate(s); got.Input != got.LocalInput || !got.Estimated {
		t.Fatalf("reset retained anchor: %+v", got)
	}
}

func TestEstimatorCompatibilityAndZeroDelta(t *testing.T) {
	mutations := map[string]func(*InputSnapshot){
		"provider":      func(s *InputSnapshot) { s.Selection.Provider += "x" },
		"model":         func(s *InputSnapshot) { s.Selection.Model.ID += "x" },
		"request model": func(s *InputSnapshot) { s.Selection.Model.BaseID = "underlying" },
		"tier":          func(s *InputSnapshot) { s.Selection.Model.ServiceTier = "priority" },
		"variant":       func(s *InputSnapshot) { s.Selection.Variant += "x" },
		"lineage":       func(s *InputSnapshot) { s.Lineage += "x" },
		"system":        func(s *InputSnapshot) { s.System += "x" },
		"tools":         func(s *InputSnapshot) { s.Tools = []llm.ToolDefinition{{Name: "new"}} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var e Estimator
			s := estimatorSnapshot()
			e.ObserveRequest(1, e.Prepare(s))
			e.ObserveUsage(1, 700)
			mutate(&s)
			if r := e.Estimate(s); !r.Estimated || r.Input != r.LocalInput {
				t.Fatalf("incompatible calibration: %+v", r)
			}
		})
	}
	var e Estimator
	s := estimatorSnapshot()
	e.ObserveRequest(1, e.Prepare(s))
	e.ObserveUsage(1, 0)
	s.Messages[0].Content = "world"
	if r := e.Estimate(s); !r.Estimated || r.Input != 0 {
		t.Fatalf("same count must not imply exact match: %+v", r)
	}
}

func TestEstimatorSignedDeltaAndInvalidCalibration(t *testing.T) {
	for _, reported := range []int{0, 1000, int(^uint(0) >> 1)} {
		var e Estimator
		s := estimatorSnapshot()
		s.Messages[0].Content = strings.Repeat("x", 300)
		before := e.ObserveRequest(1, e.Prepare(s))
		e.ObserveUsage(1, reported)
		s.Messages[0].Content = "x"
		r := e.Estimate(s)
		want := reported + r.LocalInput - before.LocalInput
		if want < 0 {
			want = r.LocalInput
		}
		if r.Input != want || !r.Estimated {
			t.Fatalf("reported %d: %+v want %d", reported, r, want)
		}
		s.Messages[0].Content = strings.Repeat("x", 600)
		r = e.Estimate(s)
		if reported == int(^uint(0)>>1) && r.Input != r.LocalInput {
			t.Fatal("overflow calibration should fall back")
		}
	}
}

func TestEstimatorRetainedAndInvalidInput(t *testing.T) {
	s := estimatorSnapshot()
	s.Messages[0].InputTimeMS = 1000
	s.Messages[0].RetainedAtMS = 2000
	if got := EstimateInput(s); got.Input != Estimate(s.System)+Tokens(s.Messages) {
		t.Fatalf("retained projection mismatch: %+v", got)
	}
	s.Messages[0].InputTimeMS = 0
	var e Estimator
	if got := e.ObserveRequest(1, e.Prepare(s)); got.Input != -1 || got.LocalInput != -1 {
		t.Fatalf("invalid retained input: %+v", got)
	}
	if _, ok := e.ObserveUsage(1, 100); ok {
		t.Fatal("invalid local input acquired anchor")
	}
	s.Messages = []llm.Message{{Type: "invalid"}}
	if got := EstimateInput(s); got.Input != -1 {
		t.Fatalf("invalid semantic type: %+v", got)
	}
}

func TestEstimatorBudgetAndReplayIdentity(t *testing.T) {
	var e Estimator
	s := estimatorSnapshot()
	tokens := 45
	s.Messages = append(s.Messages, llm.Message{Type: llm.TypeAssistant, State: &llm.ReplayState{Provider: "test", Model: "model", EstimatedTokens: &tokens}})
	e.ObserveRequest(1, e.Prepare(s))
	e.ObserveUsage(1, 600)
	s.Selection.Model.Budget.OutputAllowance++
	if r := e.Estimate(s); r.Estimated || r.Input != 600 {
		t.Fatalf("budget changed actual input identity: %+v", r)
	}
	tokens++
	if r := e.Estimate(s); !r.Estimated || r.Input != 601 {
		t.Fatalf("replay annotation not fingerprinted: %+v", r)
	}
}

func TestEstimatorConcurrent(t *testing.T) {
	var e Estimator
	s := estimatorSnapshot()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				e.ObserveRequest(int64(id+1), e.Prepare(s))
				e.ObserveUsage(int64(id+1), 400)
				e.Estimate(s)
				if j%10 == 0 {
					e.Reset()
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestFitsInput(t *testing.T) {
	b := llm.Budget{ContextLimit: 100, OutputAllowance: 20, EstimationMargin: 10, NextTurnInputReserve: 5}
	if !FitsInput(69, b, false) || FitsInput(70, b, false) || !FitsInput(64, b, true) || FitsInput(65, b, true) || FitsInput(-1, b, false) || FitsInput(int(^uint(0)>>1), b, false) {
		t.Fatal("incorrect strict budget boundary")
	}
}

type replayEstimateFunc func(llm.Message) int

func (f replayEstimateFunc) EstimateReplay(message llm.Message) int { return f(message) }

func (f replayEstimateFunc) TokenizerID(model llm.ModelSpec) string { return model.RequestID() }

func TestEstimatorOwnsReplayProjection(t *testing.T) {
	s := estimatorSnapshot()
	state := &llm.ReplayState{Provider: "test", Model: "model", Items: []json.RawMessage{json.RawMessage(`{"opaque":"state"}`)}}
	s.Messages = []llm.Message{{Type: llm.TypeAssistant, Content: "canonical", State: state}}
	calls := 0
	s.TokenEstimator = replayEstimateFunc(func(message llm.Message) int {
		calls++
		if message.State != state {
			t.Fatal("codec did not receive native replay")
		}
		return 500
	})
	var e Estimator
	observation := e.Prepare(s)
	if observation.Input != Estimate(s.System)+500 || calls != 1 {
		t.Fatalf("observation = %+v, calls = %d", observation, calls)
	}
	observation.Result = Result{}
	if r := e.ObserveRequest(1, observation); r.Input != Estimate(s.System)+500 || calls != 1 {
		t.Fatalf("prepared binding rescanned or trusted mutable result: %+v; calls %d", r, calls)
	}
	if state.EstimatedTokens != nil || s.Messages[0].State != state {
		t.Fatal("estimation mutated canonical history")
	}
	s.Selection.Model.ID = "other"
	if r := EstimateInput(s); r.Input != Estimate(s.System)+16+Estimate("canonical") || calls != 1 {
		t.Fatalf("incompatible native state was not projected away: %+v", r)
	}
}

func TestEstimatorCheckpointRoundTripAndRebinding(t *testing.T) {
	var original, restored Estimator
	s := estimatorSnapshot()
	observation := original.Prepare(s)
	checkpoint := observation.Checkpoint()
	if checkpoint == nil {
		t.Fatal("missing checkpoint")
	}
	data, err := json.Marshal(checkpoint)
	if err != nil || len(data) > 256 {
		t.Fatal("checkpoint is not bounded", len(data), err)
	}
	var decoded Checkpoint
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	s.Lineage = "loaded-copy"
	if err := restored.Restore(decoded, 900, s.Lineage); err != nil {
		t.Fatal(err)
	}
	if result := restored.Estimate(s); result.Input != 900 || result.Estimated {
		t.Fatal(result)
	}
	s.Messages = append(s.Messages, llm.Message{Type: llm.TypeUserInput, Content: "continued"})
	if result := restored.Estimate(s); result.Input != 900+result.LocalInput-observation.LocalInput || !result.Estimated {
		t.Fatal(result)
	}
	if _, ok := restored.ObserveUsage(1, 1); ok {
		t.Fatal("restore created pending request")
	}
	s.Lineage = "compacted"
	if result := restored.Estimate(s); result.Input != result.LocalInput || !result.Estimated {
		t.Fatal("checkpoint crossed reset", result)
	}
}

func TestEstimatorRejectsInvalidCheckpoint(t *testing.T) {
	var e Estimator
	s := estimatorSnapshot()
	checkpoint := e.Prepare(s).Checkpoint()
	for _, mutate := range []func(*Checkpoint){
		func(c *Checkpoint) { c.Version++ },
		func(c *Checkpoint) { c.LocalInput = -1 },
		func(c *Checkpoint) { c.Compatible = "not hex" },
		func(c *Checkpoint) { c.Exact = "00" },
	} {
		invalid := *checkpoint
		mutate(&invalid)
		if err := e.Restore(invalid, 900, s.Lineage); err == nil {
			t.Fatal("accepted invalid checkpoint", invalid)
		}
	}
	if err := e.Restore(*checkpoint, -1, s.Lineage); err == nil {
		t.Fatal("accepted negative measured input")
	}
	if (Observation{}).Checkpoint() != nil {
		t.Fatal("zero observation persisted")
	}
}

func TestEstimatorApproximateRestorationNeverExact(t *testing.T) {
	var e Estimator
	s := estimatorSnapshot()
	if !e.RestoreApproximate(s, 900) {
		t.Fatal("restore failed")
	}
	if result := e.Estimate(s); result.Input != 900 || !result.Estimated {
		t.Fatal(result)
	}
	before := EstimateInput(s).Input
	s.Messages = append(s.Messages, llm.Message{Type: llm.TypeUserInput, Content: "continued"})
	if result := e.Estimate(s); result.Input != 900+result.LocalInput-before || !result.Estimated {
		t.Fatal(result)
	}
}

type sharedTokenizer struct{ replayEstimateFunc }

func (sharedTokenizer) TokenizerID(llm.ModelSpec) string { return "shared-version" }

func TestEstimatorSharedTokenizerAcrossModelIDs(t *testing.T) {
	var e Estimator
	s := estimatorSnapshot()
	s.TokenEstimator = sharedTokenizer{replayEstimateFunc(func(llm.Message) int { return 0 })}
	e.ObserveRequest(1, e.Prepare(s))
	e.ObserveUsage(1, 900)
	s.Selection.Model.ID = "another-variant"
	if result := e.Estimate(s); result.Input != 900 || result.Estimated {
		t.Fatal("same-tokenizer switch lost calibration", result)
	}
	// Actual model input still controls identity, even for a shared tokenizer.
	s.Messages[0].Content += " new text"
	if result := e.Estimate(s); result.Input <= 900 || !result.Estimated {
		t.Fatal(result)
	}
}
