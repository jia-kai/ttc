package session

import (
	"errors"
	"sync"

	contextbuild "ttc/internal/context"
)

var errInputCancelled = errors.New("initial input cancelled before admission")

// InputAdmission keeps a human input cancellable until its initial turn commits.
// The runtime releases its text and immutable attachment snapshots on admission,
// cancellation, or RunInput returning. It is transient and must not be reused.
type InputAdmission struct {
	mu         sync.Mutex
	runtime    *Runtime
	generation uint64
	input      contextbuild.Input
	pending    bool
}

// PrepareInput transfers an immutable input to a cancellable admission ticket.
// Create the ticket before launching RunInput so cancellation cannot miss the
// handoff from a frontend queue to the runtime.
func (r *Runtime) PrepareInput(input contextbuild.Input) *InputAdmission {
	return &InputAdmission{runtime: r, generation: r.Generation(), input: input, pending: true}
}

// Cancel removes an input only if initial turn admission has not won the race.
// It returns the original text and snapshots without interrupting an active turn.
// Tickets from an explicitly changed session are no longer cancellable.
func (p *InputAdmission) Cancel() (contextbuild.Input, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.pending || p.generation != p.runtime.Generation() {
		return contextbuild.Input{}, errors.New("no queued prompt to cancel")
	}
	input := p.input
	p.clearLocked()
	return input, nil
}

func (p *InputAdmission) clearLocked() {
	p.input = contextbuild.Input{}
	p.pending = false
}

// RunInput executes a prepared human turn. Cancellation before admission returns
// nil and writes no input, turn, or model request. Admission and Cancel arbitrate
// under the ticket lock inside the workspace gate; cancellation never interrupts
// a turn that has already been admitted.
func (r *Runtime) RunInput(p *InputAdmission) error {
	if p == nil || p.runtime != r {
		return errors.New("input admission belongs to a different runtime")
	}
	defer func() {
		p.mu.Lock()
		p.clearLocked()
		p.mu.Unlock()
	}()
	p.mu.Lock()
	pending := p.pending && p.generation == r.Generation()
	p.mu.Unlock()
	if !pending {
		return nil
	}
	err := r.run(nil, p)
	if errors.Is(err, errInputCancelled) {
		return nil
	}
	return err
}
