package session

import (
	"fmt"
	"reflect"
	"ttc/internal/llm"
)

// EasyModelChoice returns the optional easy selection as an independent snapshot.
func (r *Runtime) EasyModelChoice() *llm.Selection {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.easyModel == nil {
		return nil
	}
	selection := *r.easyModel
	return &selection
}

// ProviderID identifies the backing provider whose catalog and preferences this runtime uses.
func (r *Runtime) ProviderID() string { return r.providerID }

// RestoreEasyModel installs a resolved saved choice at startup without writing preferences.
func (r *Runtime) RestoreEasyModel(selection llm.Selection) error {
	r.easyModelMu.Lock()
	defer r.easyModelMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if selection.Provider != r.providerID {
		return fmt.Errorf("easy model provider mismatch")
	}
	r.easyModel = &selection
	return nil
}

// ClearEasyModelForLogin clears the backing provider's saved easy choice before
// authorization, even while the runtime displays a different archived provider.
func (r *Runtime) ClearEasyModelForLogin() error {
	r.easyModelMu.Lock()
	defer r.easyModelMu.Unlock()
	if err := r.Store.SaveEasySelection(r.providerID, nil); err != nil {
		return err
	}
	r.mu.Lock()
	if r.easyModel != nil && r.easyModel.Provider == r.providerID {
		r.easyModel = nil
	}
	r.mu.Unlock()
	return nil
}

// RequestEasyModel persists and applies an easy selection; nil clears it.
// Already-running requests retain their own selection snapshots.
func (r *Runtime) RequestEasyModel(selection *llm.Selection) error {
	r.easyModelMu.Lock()
	defer r.easyModelMu.Unlock()
	provider := r.providerID
	if selection != nil && selection.Provider != provider {
		return fmt.Errorf("easy model provider mismatch")
	}
	if err := r.Store.SaveEasySelection(provider, selection); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if selection == nil {
		r.easyModel = nil
	} else {
		choice := *selection
		r.easyModel = &choice
	}
	return nil
}

// CurrentSelection returns the immutable selection used by the most recent request.
func (r *Runtime) CurrentSelection() llm.Selection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.selection
}

// ModelChoice returns the latest queued selection, or the current selection.
func (r *Runtime) ModelChoice() llm.Selection {
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
func (r *Runtime) RequestModel(selection llm.Selection) error {
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
	// An explicit re-selection can adopt refreshed catalog metadata even when
	// the model ID and reasoning variant have not changed.
	if reflect.DeepEqual(next, previous) {
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
		// Calibration compatibility is checked against the provider's tokenizer
		// identity on admission; changing a picker ID need not change tokenization.
	}
	r.mu.Unlock()
	if err != nil {
		return Event{}, err
	}
	return Event{Kind: "status", Text: text, EntryID: entry, SessionID: session}, nil
}
