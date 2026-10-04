package session

import (
	"fmt"
	"ttc/internal/provider"
)

// CurrentSelection returns the immutable selection used by the most recent request.
func (r *Runtime) CurrentSelection() provider.Selection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.selection
}

// ModelChoice returns the latest queued selection, or the current selection.
func (r *Runtime) ModelChoice() provider.Selection {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingModel != nil {
		return *r.pendingModel
	}
	return r.selection
}

// RequestModel remembers and queues a validated catalog choice. The latest
// explicit choice wins, even if applying its history record later fails.
// ApplyModel is called by the turn owner at a request boundary, or by an idle frontend.
func (r *Runtime) RequestModel(selection provider.Selection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.Store.SaveSelection(selection); err != nil {
		return err
	}
	r.pendingModel = &selection
	return nil
}

// ApplyModel persists a queued switch before making it current. Call only between
// requests/tool batches or while idle. Existing requests and children keep their snapshots.
// The caller presents a returned event with nonempty Kind. Blank conversations
// have no history entry for a switch, so their event has a zero EntryID. This method
// never emits into a frontend-owned queue while that frontend is applying a switch.
func (r *Runtime) ApplyModel(turn string) (Event, error) {
	r.mu.Lock()
	if r.pendingModel == nil {
		r.mu.Unlock()
		return Event{}, nil
	}
	next, previous := *r.pendingModel, r.selection
	if next.Provider == previous.Provider && next.Model.ID == previous.Model.ID && next.Variant == previous.Variant {
		r.pendingModel = nil
		r.mu.Unlock()
		return Event{}, nil
	}
	text := fmt.Sprintf("Model switched · %s · %s", next.Model.ID, next.Variant)
	session := r.current
	var entry int64
	var err error
	if r.persisted {
		entry, err = r.Store.SwitchModel(session, turn, previous, next, text)
	}
	if err == nil {
		r.selection, r.pendingModel = next, nil
	}
	r.mu.Unlock()
	if err != nil {
		return Event{}, err
	}
	return Event{Kind: "status", Text: text, EntryID: entry, SessionID: session}, nil
}
