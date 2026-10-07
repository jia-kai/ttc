package session

import (
	"errors"
	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"unicode/utf8"
)

// Steer queues a human instruction for the next settled main request boundary.
// It creates no durable position or undo checkpoint until request admission.
func (r *Runtime) Steer(input contextbuild.Input) error {
	if err := validateSteeringInput(input); err != nil {
		return err
	}
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	r.mu.Lock()
	active := r.activeTurn != "" && r.activeCancel != nil
	fatal := r.fatalCompaction
	r.mu.Unlock()
	if !active {
		return errors.New("main turn is not ready for steering; send or queue an ordinary message")
	}
	if fatal != "" {
		return errors.New("session unusable after compaction; start or load another session")
	}
	r.steers = append(r.steers, input)
	return nil
}

func validateSteeringInput(input contextbuild.Input) error {
	if !utf8.ValidString(input.Text) || input.Text == "" && len(input.Attachments) == 0 {
		return errors.New("steering requires a human UTF-8 user instruction or attachment")
	}
	for _, attachment := range input.Attachments {
		if !utf8.ValidString(attachment.Path) || attachment.File == nil && (!utf8.ValidString(attachment.Kind) || !utf8.ValidString(attachment.Text)) {
			return errors.New("steering attachment must contain valid UTF-8")
		}
	}
	return nil
}

// CancelSteer removes and returns the newest unadmitted steering input. Admission
// and cancellation share a gate: an already admitted instruction cannot be removed.
// It creates no history entry or undo checkpoint and leaves older steers intact.
func (r *Runtime) CancelSteer() (contextbuild.Input, error) {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	if len(r.steers) == 0 {
		return contextbuild.Input{}, errors.New("no pending steering instruction to cancel")
	}
	last := len(r.steers) - 1
	input := r.steers[last]
	r.steers[last] = contextbuild.Input{}
	r.steers = r.steers[:last]
	return input, nil
}

// SteeringPreview returns the pending count and at most limit original texts,
// oldest first. It does not copy attachments or expand model-facing messages.
func (r *Runtime) SteeringPreview(limit int) (int, []string) {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	texts := make([]string, min(max(0, limit), len(r.steers)))
	for i := range texts {
		texts[i] = r.steers[i].Text
	}
	return len(r.steers), texts
}

func (r *Runtime) steeringMessagesLocked() []llm.Message {
	messages := make([]llm.Message, len(r.steers))
	for i, input := range r.steers {
		messages[i] = input.Message()
	}
	return messages
}
