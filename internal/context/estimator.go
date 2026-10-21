package context

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"sync"

	"ttc/internal/llm"
)

// ReplayEstimator supplies provider-native replay occupancy in input tokens.
// Implementations must be concurrency-safe when shared across estimator calls.
type ReplayEstimator interface {
	EstimateReplay(llm.Message) int
}

// InputSnapshot is an immutable model-input view for one estimation. Messages
// are canonical history; estimation owns provider projection and replay counts.
// Lineage must change after a context reset or compaction; no prefix is inferred.
// Callers must not mutate its referenced data while an estimator call is running.
type InputSnapshot struct {
	Selection llm.Selection
	System    string
	Tools     []llm.ToolDefinition
	Messages  []llm.Message
	Lineage   string
	// ReplayEstimator is optional; nil uses llm.ReplayTokens conservatively.
	ReplayEstimator ReplayEstimator
}

// Result separates effective input occupancy from its local estimate. Input and
// LocalInput are -1 for invalid input. Estimated is false only for an exact
// snapshot with endpoint feedback. Output reserves belong to the caller.
type Result struct {
	Input      int
	LocalInput int
	Estimated  bool
}

// Observation is a payload-free prepared input estimate. Its embedded Result is
// the estimate at preparation time; private fingerprints bind later feedback.
// A zero Observation is invalid. Observations may be passed by value.
type Observation struct {
	Result
	identity inputIdentity
	local    int
	valid    bool
}

// EstimateInput estimates the complete input without calibration or state.
// Invalid message or retained-input metadata returns -1.
func EstimateInput(snapshot InputSnapshot) Result {
	snapshot.Messages = MessagesForEstimation(snapshot.Selection, snapshot.Messages, snapshot.ReplayEstimator)
	return estimateLocal(snapshot)
}

// MessagesForEstimation copies the selected provider projection and annotates
// replay occupancy without changing canonical history or provider payloads.
// With no estimator, existing annotations or conservative replay counts are used.
func MessagesForEstimation(selection llm.Selection, messages []llm.Message, estimator ReplayEstimator) []llm.Message {
	out := llm.ContextFor(selection, messages)
	for i, message := range out {
		if message.State == nil {
			continue
		}
		state := *message.State
		tokens := llm.ReplayTokens(message.State)
		if estimator != nil {
			tokens = max(0, estimator.EstimateReplay(message))
		}
		state.EstimatedTokens = &tokens
		out[i].State = &state
	}
	return out
}

type inputIdentity struct {
	compatible [sha256.Size]byte
	exact      [sha256.Size]byte
}

type inputObservation struct {
	identity inputIdentity
	result   Result
	request  int64
}

type inputAnchor struct {
	identity inputIdentity
	local    int
	reported int
}

// Estimator calibrates local token deltas against one latest reported request.
// Its zero value is ready to use. All methods are concurrency-safe; instances
// belong to one independently managed context, not a registry of actors. State
// retains only fingerprints, counts and one pending request, never input payloads.
// An Estimator must not be copied after first use.
type Estimator struct {
	mu      sync.Mutex
	anchor  *inputAnchor
	pending *inputObservation
}

// Prepare projects and fingerprints input once for admission and later request
// binding. The observation retains no payloads or replay-estimator dependency.
func (e *Estimator) Prepare(snapshot InputSnapshot) Observation {
	r, identity, ok := estimateSnapshot(snapshot)
	e.mu.Lock()
	defer e.mu.Unlock()
	return Observation{Result: e.calibrate(r, identity), identity: identity, local: r.LocalInput, valid: ok}
}

// ObserveRequest binds a prepared input without rescanning it, replacing the
// single pending request. IDs must be positive and unique between Reset calls.
// Missing feedback preserves the successful anchor. Invalid observations clear
// the pending request; changing the observation's public Result has no effect.
func (e *Estimator) ObserveRequest(requestID int64, observation Observation) Result {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending = nil
	if !observation.valid {
		return Result{Input: -1, LocalInput: -1, Estimated: true}
	}
	local := Result{Input: observation.local, LocalInput: observation.local, Estimated: true}
	r := e.calibrate(local, observation.identity)
	if requestID > 0 {
		e.pending = &inputObservation{identity: observation.identity, result: local, request: requestID}
	}
	return r
}

// ObserveUsage commits nonnegative endpoint input tokens for the pending ID and
// returns its exact result (Estimated=false). Unknown/superseded IDs, duplicate
// reports and negative tokens return (Result{}, false) without changing state.
func (e *Estimator) ObserveUsage(requestID int64, inputTokens int) (Result, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if inputTokens < 0 || e.pending == nil || e.pending.request != requestID {
		return Result{}, false
	}
	p := e.pending
	e.anchor = &inputAnchor{identity: p.identity, local: p.result.LocalInput, reported: inputTokens}
	e.pending = nil
	r := p.result
	r.Input = inputTokens
	r.Estimated = false
	return r, true
}

// Estimate returns an estimate without changing the pending request or anchor.
// Compatible snapshots use reported input plus the signed local delta. Negative
// or overflowing calibrated values fall back to the uncalibrated local estimate.
func (e *Estimator) Estimate(snapshot InputSnapshot) Result {
	return e.Prepare(snapshot).Result
}

// Reset forgets both the pending request and calibration anchor.
func (e *Estimator) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.anchor = nil
	e.pending = nil
}

func (e *Estimator) calibrate(r Result, identity inputIdentity) Result {
	a := e.anchor
	if a == nil || identity.compatible != a.identity.compatible {
		return r
	}
	if identity.exact == a.identity.exact {
		r.Input, r.Estimated = a.reported, false
		return r
	}
	delta := r.LocalInput - a.local
	maxInt := int(^uint(0) >> 1)
	if (delta >= 0 && a.reported <= maxInt-delta) || (delta < 0 && a.reported >= -delta) {
		r.Input = a.reported + delta
	}
	return r
}

func estimateLocal(snapshot InputSnapshot) Result {
	invalid := Result{Input: -1, LocalInput: -1, Estimated: true}
	projected, err := ProjectRetainedInputs(snapshot.Messages)
	if err != nil {
		return invalid
	}
	n := rawTokens(projected)
	if n < 0 {
		return invalid
	}
	n += Estimate(snapshot.System)
	for _, d := range snapshot.Tools {
		n += 16 + Estimate(d.Description) + Estimate(string(d.Parameters))
	}
	return Result{Input: n, LocalInput: n, Estimated: true}
}

func rawTokens(messages []llm.Message) int {
	n := 0
	for _, m := range messages {
		content, err := m.ModelContent()
		if err != nil {
			return -1
		}
		if m.State != nil {
			n += llm.ReplayTokens(m.State)
			continue
		}
		n += 16 + Estimate(content)
		for _, c := range m.Calls {
			n += 16 + Estimate(c.Name) + Estimate(string(c.Arguments))
		}
		for _, file := range m.Files {
			n += file.EstimatedTokens()
		}
	}
	return n
}

func estimateSnapshot(snapshot InputSnapshot) (Result, inputIdentity, bool) {
	snapshot.Messages = MessagesForEstimation(snapshot.Selection, snapshot.Messages, snapshot.ReplayEstimator)
	r := estimateLocal(snapshot)
	if r.Input < 0 {
		return r, inputIdentity{}, false
	}
	// Length-prefixed fields avoid allocating serialized copies of large payloads.
	compatible := fingerprintWriter{Hash: sha256.New()}
	for _, field := range []string{snapshot.Selection.Provider, snapshot.Selection.Model.ID,
		snapshot.Selection.Model.RequestID(), snapshot.Selection.Model.ServiceTier,
		snapshot.Selection.Variant, snapshot.Lineage, snapshot.System} {
		compatible.string(field)
	}
	compatible.integer(int64(len(snapshot.Tools)))
	for _, tool := range snapshot.Tools {
		compatible.string(tool.Name)
		compatible.string(tool.Description)
		compatible.bytes(tool.Parameters)
	}
	exact := fingerprintWriter{Hash: sha256.New()}
	_, _ = exact.Write(compatible.Sum(nil))
	exact.integer(int64(len(snapshot.Messages)))
	for _, m := range snapshot.Messages {
		for _, field := range []string{string(m.Type), m.Phase, m.Content, m.CallID, m.InputSource} {
			exact.string(field)
		}
		exact.integer(m.InputTimeMS)
		exact.integer(m.RetainedAtMS)
		exact.integer(int64(len(m.Calls)))
		for _, call := range m.Calls {
			exact.string(call.ID)
			exact.string(call.Name)
			exact.bytes(call.Arguments)
		}
		exact.integer(int64(len(m.Files)))
		for _, file := range m.Files {
			exact.string(file.Path)
			exact.string(file.SHA256)
			exact.string(file.MIMEType)
			exact.integer(int64(file.Bytes))
		}
		if m.State == nil {
			exact.integer(0)
			continue
		}
		exact.integer(1)
		exact.string(m.State.Provider)
		exact.string(m.State.Model)
		exact.integer(int64(m.State.Version))
		exact.integer(int64(llm.ReplayTokens(m.State)))
		exact.integer(int64(len(m.State.Items)))
		for _, item := range m.State.Items {
			exact.bytes(item)
		}
	}
	var identity inputIdentity
	copy(identity.compatible[:], compatible.Sum(nil))
	copy(identity.exact[:], exact.Sum(nil))
	return r, identity, true
}

// fingerprintWriter reuses one scratch allocation across all fields in a hash.
type fingerprintWriter struct {
	hash.Hash
	buffer [4096]byte
}

func (w *fingerprintWriter) string(value string) {
	w.integer(int64(len(value)))
	for len(value) > 0 {
		n := copy(w.buffer[:], value)
		_, _ = w.Write(w.buffer[:n])
		value = value[n:]
	}
}

func (w *fingerprintWriter) bytes(value []byte) {
	w.integer(int64(len(value)))
	_, _ = w.Write(value)
}

func (w *fingerprintWriter) integer(value int64) {
	binary.LittleEndian.PutUint64(w.buffer[:8], uint64(value))
	_, _ = w.Write(w.buffer[:8])
}

// FitsInput reports whether nonnegative input tokens fit strictly below the
// context limit after output/safety reserves and optional next-turn headroom.
// Invalid input or negative budget fields do not fit. Subtraction avoids overflow.
func FitsInput(input int, budget llm.Budget, headroom bool) bool {
	remaining := budget.ContextLimit
	reserves := []int{input, budget.OutputAllowance, budget.EstimationMargin}
	if headroom {
		reserves = append(reserves, budget.NextTurnInputReserve)
	}
	for _, reserve := range reserves {
		if reserve < 0 || reserve >= remaining {
			return false
		}
		remaining -= reserve
	}
	return true
}
