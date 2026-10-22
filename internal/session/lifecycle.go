package session

import (
	"errors"
	"fmt"
	"time"

	"ttc/internal/history"
	"ttc/internal/llm"
)

// preparedSession contains activation state derived from validated history.
// Store.Load freezes the source boundary before accounting preparation, which
// retains no history payloads. Installing it needs no storage I/O; joining old
// work can still publish source-owned completions.
type preparedSession struct {
	session     history.Session
	selection   llm.Selection
	notices     []llm.Message
	accounting  preparedAccounting
	switchEntry int64
	switchText  string
}

// prepareSession either attaches an already persisted target (New's contract),
// or snapshots an explicit load. All fallible operations precede live teardown.
func (r *Runtime) prepareSession(id string, copySource bool) (preparedSession, error) {
	v, err := r.Store.Session(id)
	if err != nil {
		return preparedSession{}, err
	}
	if v.WorkspaceID != "" {
		path, err := r.Store.WorkspacePath(r.ctx, v.WorkspaceID)
		if err != nil {
			return preparedSession{}, err
		}
		if path != r.Workspace.Root {
			return preparedSession{}, errors.New("session belongs to another workspace")
		}
	}
	if copySource {
		v, err = r.Store.Load(id)
		if err != nil {
			return preparedSession{}, err
		}
	}
	target := preparedSession{session: v, selection: r.CurrentSelection()}
	if v.ReadOnly {
		target.selection = v.Model
	}
	pending, err := r.Store.PendingRecoveryNotifications(r.ctx, v.ID)
	if err == nil {
		target.notices, err = recoveredNotificationMessages(pending)
	}
	if err != nil {
		return preparedSession{}, fmt.Errorf("read recovered notifications: %w", err)
	}
	target.accounting, err = r.prepareAccounting(v.ID, target.selection)
	if err != nil {
		return preparedSession{}, err
	}
	if copySource && !v.ReadOnly && (v.Model.Provider != target.selection.Provider || v.Model.Model.ID != target.selection.Model.ID || v.Model.Variant != target.selection.Variant) {
		target.switchText = fmt.Sprintf("Model switched · %s · %s", target.selection.Model.ID, target.selection.Variant)
		target.switchEntry, err = r.Store.SwitchModel(v.ID, "", v.Model, target.selection, target.switchText)
		if err != nil {
			return preparedSession{}, err
		}
	}
	if err := r.Store.RefreshLoadedActivity(r.ctx, time.Now(), v.ID); err != nil {
		return preparedSession{}, err
	}
	return target, nil
}

func (r *Runtime) stopLiveWork() {
	r.stopNaming()
	r.Jobs.Close()
	r.timers.close()
	r.clearImages()
}

func (r *Runtime) activateSession(target preparedSession) {
	r.stopLiveWork()
	r.resetLiveWork()
	r.installSession(target)
	if target.switchEntry != 0 {
		r.emit(Event{Kind: "status", Text: target.switchText, EntryID: target.switchEntry, SessionID: target.session.ID})
	}
}

// installSession is shared by initial attachment and an explicit activation.
// Its caller owns foreground exclusion and has already initialized live work.
func (r *Runtime) installSession(target preparedSession) {
	r.mu.Lock()
	r.current = target.session.ID
	r.persisted = true
	r.selection = target.selection
	r.mu.Unlock()
	r.replaceAccounting(target.accounting)
	r.orderMu.Lock()
	r.notifications = target.notices
	r.orderMu.Unlock()
}

// Load snapshots a stored conversation and activates it under the current writable
// model selection. Archived sessions retain their saved selection. Failed
// preparation leaves the active conversation and its live work unchanged.
func (r *Runtime) Load(id string) error {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	_, err := r.load(id)
	return err
}

func (r *Runtime) load(id string) (history.Session, error) {
	target, err := r.prepareSession(id, true)
	if err != nil {
		return history.Session{}, err
	}
	r.activateSession(target)
	return target.session, nil
}
