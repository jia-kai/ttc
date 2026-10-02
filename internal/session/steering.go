package session

import (
	"errors"
	"scicode/internal/provider"
	"unicode/utf8"
)

// Steer queues a human instruction for the next settled main request boundary.
// It creates no durable position or undo checkpoint until request admission.
func (r *Runtime) Steer(message provider.Message) error {
	if message.Role != "user" || message.Runtime || !utf8.ValidString(message.Content) || message.Content == "" && len(message.Images) == 0 {
		return errors.New("steering requires a human UTF-8 user instruction or image")
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
	r.steers = append(r.steers, message)
	return nil
}

// PendingSteers returns composer previews; callers must not mutate image data.
func (r *Runtime) PendingSteers() []provider.Message {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	return append([]provider.Message(nil), r.steers...)
}
