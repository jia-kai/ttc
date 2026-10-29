package context

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"sync"

	"ttc/internal/llm"
)

// TokenEstimator supplies tokenizer compatibility and native replay occupancy.
// Implementations must be concurrency-safe when shared across estimator calls.
type TokenEstimator interface {
	EstimateReplay(llm.Message) int
	TokenizerID(llm.ModelSpec) string
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
	// TokenEstimator is optional; nil uses exact request model IDs and conservative replay counts.
	TokenEstimator TokenEstimator
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
	snapshot.Messages = MessagesForEstimation(snapshot.Selection, snapshot.Messages, snapshot.TokenEstimator)
	return estimateLocal(snapshot)
}

// MessagesForEstimation copies the selected provider projection and annotates
// replay occupancy without changing canonical history or provider payloads.
// With no estimator, existing annotations or conservative replay counts are used.
func MessagesForEstimation(selection llm.Selection, messages []llm.Message, estimator TokenEstimator) []llm.Message {
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
	lineage    string
}

type inputObservation struct {
	identity inputIdentity
	result   Result
	request  int64
}

type inputAnchor struct {
	identity    inputIdentity
	local       int
	reported    int
	approximate bool // Reconstructed or transferred calibration is never exact.
}

// Checkpoint is a versioned, payload-free prepared-input observation. It stores
// local tokens and SHA-256 fingerprints, not messages, schemas or usage. History
// must bind it to a successful response before Restore can establish an anchor.
type Checkpoint struct {
	Version    int    `json:"version"`     // Fingerprint/estimation contract version, independent of history schema.
	LocalInput int    `json:"local_input"` // Local input estimate, in tokens, before endpoint calibration.
	Compatible string `json:"compatible"`  // Hex SHA-256 of tokenizer, provider, settings, system and schemas.
	Exact      string `json:"exact"`       // Hex SHA-256 additionally covering the projected message payloads.
}

// Checkpoint returns bounded persistence metadata for this prepared input.
// Invalid observations return nil.
func (o Observation) Checkpoint() *Checkpoint {
	if !o.valid {
		return nil
	}
	return &Checkpoint{Version: 1, LocalInput: o.local,
		Compatible: hex.EncodeToString(o.identity.compatible[:]), Exact: hex.EncodeToString(o.identity.exact[:])}
}

// Validate checks the format version, token count and fingerprint encodings
// without changing estimator state or establishing historical ownership.
func (c Checkpoint) Validate() error {
	_, err := c.identity()
	return err
}

func (c Checkpoint) identity() (inputIdentity, error) {
	if c.Version != 1 || c.LocalInput < 0 {
		return inputIdentity{}, errors.New("invalid token checkpoint version or count")
	}
	var identity inputIdentity
	for _, field := range []struct {
		text   string
		target *[sha256.Size]byte
	}{{c.Compatible, &identity.compatible}, {c.Exact, &identity.exact}} {
		decoded, err := hex.DecodeString(field.text)
		if err != nil || len(decoded) != sha256.Size {
			return inputIdentity{}, errors.New("invalid token checkpoint fingerprint")
		}
		copy(field.target[:], decoded)
	}
	return identity, nil
}

// Restore installs a validated historical observation and nonnegative measured
// input, rebinding it to a selected load's context. The caller must establish
// selected-branch ownership and reject observations from before compaction.
// Pending requests are never restored. Invalid metadata leaves state unchanged.
func (e *Estimator) Restore(c Checkpoint, inputTokens int, lineage string) error {
	identity, err := c.identity()
	if err != nil {
		return err
	}
	if inputTokens < 0 {
		return errors.New("invalid measured input token count")
	}
	identity.lineage = lineage
	e.mu.Lock()
	defer e.mu.Unlock()
	e.anchor = &inputAnchor{identity: identity, local: c.LocalInput, reported: inputTokens}
	e.pending = nil
	return nil
}

// RestoreApproximate reconstructs an older request using current token accounting
// policy and tool schemas. Its measurement calibrates deltas but can never mark
// an input exact. Invalid input leaves state unchanged and returns false.
func (e *Estimator) RestoreApproximate(snapshot InputSnapshot, inputTokens int) bool {
	r, identity, ok := estimateSnapshot(snapshot)
	if !ok || inputTokens < 0 {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.anchor = &inputAnchor{identity: identity, local: r.LocalInput, reported: inputTokens, approximate: true}
	e.pending = nil
	return true
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

// Fork transfers the last completed calibration to an independently owned
// estimator, replacing only source's system, tools and lineage. The target is
// always approximate, including when its input is identical to source. Role and
// schema changes use their signed local token delta, not new endpoint feedback.
// The caller must preserve source's selected shared input; later actor-local
// messages use ordinary local deltas. Only counts and fingerprints are retained,
// with no pending request or shared mutable state.
//
// Fork returns nil for a nil receiver, empty source or target lineage, invalid
// input, missing/incompatible completed calibration, or negative/overflowing
// calibrated counts. A pending request is neither transferred nor used.
func (e *Estimator) Fork(source InputSnapshot, system string, tools []llm.ToolDefinition, lineage string) *Estimator {
	if e == nil || source.Lineage == "" || lineage == "" {
		return nil
	}
	e.mu.Lock()
	if e.anchor == nil {
		e.mu.Unlock()
		return nil
	}
	a := *e.anchor
	e.mu.Unlock()
	if source.Lineage != a.identity.lineage || a.local < 0 || a.reported < 0 {
		return nil
	}
	compatible, ok := compatibilityFingerprint(source)
	if !ok || compatible != a.identity.compatible {
		return nil
	}
	// Account for and fingerprint shared messages only once. Incompatible
	// sources are rejected before projecting replay or scanning large payloads.
	source.Messages = MessagesForEstimation(source.Selection, source.Messages, source.TokenEstimator)
	r, identity, ok := estimateProjectedSnapshot(source, compatible)
	if !ok {
		return nil
	}
	input := a.reported
	if a.approximate || identity.exact != a.identity.exact {
		input, ok = calibratedInput(a.reported, r.LocalInput, a.local)
		if !ok {
			return nil
		}
	}
	target := source
	target.System, target.Tools, target.Lineage = system, tools, lineage
	targetCompatible, ok := compatibilityFingerprint(target)
	targetLocal := sumTokens(r.LocalInput-fixedInputTokens(source.System, source.Tools), fixedInputTokens(system, tools))
	if !ok || targetLocal < 0 {
		return nil
	}
	input, ok = calibratedInput(input, targetLocal, r.LocalInput)
	if !ok {
		return nil
	}
	// Approximate anchors never compare exact fingerprints. The first target
	// request computes its own full identity for any later measured feedback.
	targetIdentity := inputIdentity{compatible: targetCompatible, lineage: lineage}
	return &Estimator{anchor: &inputAnchor{identity: targetIdentity, local: targetLocal, reported: input, approximate: true}}
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
	if a == nil || r.Input < 0 || identity.compatible != a.identity.compatible || identity.lineage != a.identity.lineage {
		return r
	}
	if !a.approximate && identity.exact == a.identity.exact {
		r.Input, r.Estimated = a.reported, false
		return r
	}
	if input, ok := calibratedInput(a.reported, r.LocalInput, a.local); ok {
		r.Input = input
	}
	return r
}

func calibratedInput(reported, local, baseline int) (int, bool) {
	if reported < 0 || local < 0 || baseline < 0 {
		return 0, false
	}
	delta := local - baseline
	maxInt := int(^uint(0) >> 1)
	if (delta >= 0 && reported > maxInt-delta) || (delta < 0 && reported < -delta) {
		return 0, false
	}
	return reported + delta, true
}

// sumTokens propagates invalid counts and rejects overflow before addition.
func sumTokens(counts ...int) int {
	n := 0
	for _, count := range counts {
		if count < 0 || count > int(^uint(0)>>1)-n {
			return -1
		}
		n += count
	}
	return n
}

func estimateLocal(snapshot InputSnapshot) Result {
	invalid := Result{Input: -1, LocalInput: -1, Estimated: true}
	projected, err := ProjectRetainedInputs(snapshot.Messages)
	if err != nil {
		return invalid
	}
	n := sumTokens(rawTokens(projected), fixedInputTokens(snapshot.System, snapshot.Tools))
	if n < 0 {
		return invalid
	}
	return Result{Input: n, LocalInput: n, Estimated: true}
}

func fixedInputTokens(system string, tools []llm.ToolDefinition) int {
	n := Estimate(system)
	for _, d := range tools {
		n = sumTokens(n, 16, Estimate(d.Description), Estimate(string(d.Parameters)))
	}
	return n
}

func rawTokens(messages []llm.Message) int {
	n := 0
	for _, m := range messages {
		content, err := m.ModelContent()
		if err != nil {
			return -1
		}
		if m.State != nil {
			n = sumTokens(n, llm.ReplayTokens(m.State))
			continue
		}
		n = sumTokens(n, 16, Estimate(content))
		for _, c := range m.Calls {
			n = sumTokens(n, 16, Estimate(c.Name), Estimate(string(c.Arguments)))
		}
		for _, file := range m.Files {
			n = sumTokens(n, file.EstimatedTokens())
		}
	}
	return n
}

func estimateSnapshot(snapshot InputSnapshot) (Result, inputIdentity, bool) {
	compatible, ok := compatibilityFingerprint(snapshot)
	if !ok {
		return Result{Input: -1, LocalInput: -1, Estimated: true}, inputIdentity{}, false
	}
	snapshot.Messages = MessagesForEstimation(snapshot.Selection, snapshot.Messages, snapshot.TokenEstimator)
	return estimateProjectedSnapshot(snapshot, compatible)
}

func compatibilityFingerprint(snapshot InputSnapshot) ([sha256.Size]byte, bool) {
	// Length-prefixed fields avoid allocating serialized copies of large payloads.
	compatible := fingerprintWriter{Hash: sha256.New()}
	tokenizer := snapshot.Selection.Model.RequestID()
	if snapshot.TokenEstimator != nil {
		tokenizer = snapshot.TokenEstimator.TokenizerID(snapshot.Selection.Model)
	}
	if tokenizer == "" {
		return [sha256.Size]byte{}, false
	}
	for _, field := range []string{snapshot.Selection.Provider, tokenizer, snapshot.Selection.Model.ServiceTier,
		snapshot.Selection.Variant, snapshot.System} {
		compatible.string(field)
	}
	compatible.integer(int64(len(snapshot.Tools)))
	for _, tool := range snapshot.Tools {
		compatible.string(tool.Name)
		compatible.string(tool.Description)
		compatible.bytes(tool.Parameters)
	}
	var fingerprint [sha256.Size]byte
	compatible.Sum(fingerprint[:0])
	return fingerprint, true
}

func estimateProjectedSnapshot(snapshot InputSnapshot, compatible [sha256.Size]byte) (Result, inputIdentity, bool) {
	r := estimateLocal(snapshot)
	if r.Input < 0 {
		return r, inputIdentity{}, false
	}
	exact := fingerprintWriter{Hash: sha256.New()}
	_, _ = exact.Write(compatible[:])
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
	identity.lineage = snapshot.Lineage
	identity.compatible = compatible
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
