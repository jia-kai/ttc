package session

import (
	"errors"

	"ttc/internal/history"
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

// RestoreBranch selects a complete main-tool boundary and restores saved
// files without reexecuting tools. Command owns foreground exclusion; this
// method cancels and joins live work before changing the cursor.
// Redo can restore the previously selected branch without rerunning tools.
func (r *Runtime) RestoreBranch(entryID int64) error {
	r.mu.Lock()
	id, persisted := r.current, r.persisted
	r.mu.Unlock()
	if !persisted {
		return errors.New("session is empty; send a message first")
	}
	if _, err := r.Store.PlanBranch(id, entryID); err != nil {
		return err
	}
	r.stopLiveWork()
	r.resetLiveWork()
	r.replaceAccounting(preparedAccounting{})
	// Joined completions may append history, so resolve against the final state.
	target, err := r.Store.PlanBranch(id, entryID)
	if err != nil {
		return err
	}
	return r.Workspace.Restore(r.ctx, target)
}
