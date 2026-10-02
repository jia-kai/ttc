package session

import (
	"errors"

	"scicode/internal/history"
)

// History snapshots all immutable branches of the current conversation. Blank,
// archived and invalid contexts remain inspectable without starting inference.
func (r *Runtime) History() (history.BranchTree, error) {
	r.mu.Lock()
	id, persisted := r.current, r.persisted
	r.mu.Unlock()
	if !persisted {
		return history.BranchTree{SessionID: id, Name: "New session", Nodes: []history.BranchNode{{Label: "Start of session", Reason: "empty session"}}}, nil
	}
	return r.Store.HistoryTree(r.ctx, id)
}

// RestoreBranch selects a complete main-tool boundary and restores journaled
// files without reexecuting tools. Command owns foreground exclusion and pauses
// retention; this method cancels and joins live work before changing the cursor.
// Redo can restore the previously selected branch without rerunning tools.
func (r *Runtime) RestoreBranch(entryID int64) error {
	r.mu.Lock()
	id, persisted := r.current, r.persisted
	r.mu.Unlock()
	if !persisted {
		return errors.New("session is empty; send a message first")
	}
	if _, err := r.Store.BranchSelectionTarget(id, entryID); err != nil {
		return err
	}
	r.stopNaming()
	r.Jobs.Close()
	r.timers.close()
	r.clearImages()
	r.resetTransient()
	// Joined completions may append history, so resolve against the final state.
	target, err := r.Store.BranchSelectionTarget(id, entryID)
	if err != nil {
		return err
	}
	saved, err := r.Store.Session(id)
	if err != nil {
		return err
	}
	if target.EntryTip != saved.EntryTip {
		target.RedoTip = saved.EntryTip
	} else {
		target.RedoTip = saved.RedoTip
	}
	return r.Workspace.Restore(r.ctx, id, "branch", target)
}
