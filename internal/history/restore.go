package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ttc/internal/llm"
)

// RestorePlan is an opaque selection of conversation and file history. Plans are
// bound to the issuing store and selected session state; callers must obtain a
// fresh plan if history changes. Planning does not change history or files.
type RestorePlan struct {
	store         *Store
	session       string
	before, after restoreCursor
}

type restoreCursor struct {
	entry, file, redo int64
}

func (s *Store) restorePlan(v Session, entry, file, redo int64) RestorePlan {
	return RestorePlan{store: s, session: v.ID,
		before: restoreCursor{v.EntryTip, v.FileTip, v.RedoTip},
		after:  restoreCursor{entry, file, redo}}
}

// PlanUndo selects the newest human user/steer turn's starting checkpoint,
// respecting the session undo floor, and saves the current branch for redo.
func (s *Store) PlanUndo(session string) (RestorePlan, error) {
	v, e := s.Session(session)
	if e != nil {
		return RestorePlan{}, e
	}
	if v.ReadOnly {
		return RestorePlan{}, errors.New("session is read-only")
	}
	entries, e := s.Branch(session, 0)
	if e != nil {
		return RestorePlan{}, e
	}
	for i := len(entries) - 1; i >= 0; i-- {
		x := entries[i]
		if x.ID <= v.UndoFloor {
			break
		}
		if x.Kind == "message" && x.Actor == "main" && x.Role == "user" && x.TurnID != "" {
			var message llm.Message
			if err := json.Unmarshal(x.Content, &message); err != nil {
				return RestorePlan{}, err
			}
			if message.Type != llm.TypeUserInput {
				continue
			}
			var start, tip sql.NullInt64
			var trigger string
			if e = s.db.QueryRow("SELECT start_entry_id,start_file_tip_id,trigger FROM turns WHERE id=? AND session_id=?", x.TurnID, session).Scan(&start, &tip, &trigger); e != nil {
				return RestorePlan{}, fmt.Errorf("undo checkpoint ownership: %w", e)
			}
			if trigger != "user" && trigger != "steer" {
				continue
			}
			if start.Int64 < v.UndoFloor {
				return RestorePlan{}, errors.New("undo would cross session boundary")
			}
			found := start.Int64 == 0
			for _, entry := range entries {
				found = found || entry.ID == start.Int64
			}
			if !found {
				return RestorePlan{}, errors.New("undo checkpoint is outside active ancestry")
			}
			return s.restorePlan(v, start.Int64, tip.Int64, v.EntryTip), nil
		}
	}
	return RestorePlan{}, errors.New("nothing to undo")
}

// PlanRedo selects the saved redo branch without executing tools. Completing it
// consumes redo; unlike explicit branch selection, redo does not toggle branches.
func (s *Store) PlanRedo(session string) (RestorePlan, error) {
	v, err := s.Session(session)
	if err != nil {
		return RestorePlan{}, err
	}
	if v.RedoTip == 0 {
		return RestorePlan{}, errors.New("nothing to redo")
	}
	plan, err := s.branchPlan(v, v.RedoTip)
	if err != nil {
		return RestorePlan{}, err
	}
	plan.after.redo = 0
	return plan, nil
}

// branchPlan resolves a node in this session's logical history. PlanBranch additionally
// validates tool boundaries and floor ancestry before exposing the plan.
func (s *Store) branchPlan(v Session, id int64) (RestorePlan, error) {
	if v.ReadOnly {
		return RestorePlan{}, errors.New("session is read-only")
	}
	if id < v.UndoFloor {
		return RestorePlan{}, errors.New("branch crosses undo boundary")
	}
	var tip int64
	if id != 0 {
		if err := validateHistoryTip(s.db, v.ID, id); err != nil {
			return RestorePlan{}, fmt.Errorf("branch target: %w", err)
		}
		base, err := forkBaseWith(s.db, v.ID)
		if err != nil {
			return RestorePlan{}, err
		}
		if id > base {
			if err := s.db.QueryRow("SELECT coalesce(file_tip_id,0) FROM entries WHERE id=?", id).Scan(&tip); err != nil {
				return RestorePlan{}, fmt.Errorf("branch target: %w", err)
			}
		}
	}
	redo := v.RedoTip
	if id != v.EntryTip {
		redo = v.EntryTip
	}
	return s.restorePlan(v, id, tip, redo), nil
}

// RestoreChange is an application-owned path-effect payload in restoration
// order. Reverse means undo the payload, including its internal effect order.
// History treats Paths as opaque JSON; workspace owns its typed interpretation.
// Storage IDs and predecessor links are deliberately not exposed.
type RestoreChange struct {
	Paths   json.RawMessage
	Reverse bool
}

type fileChange struct {
	id, previous int64
	reversible   bool
	paths        json.RawMessage
}

func (s *Store) fileChanges(tip int64) ([]fileChange, error) {
	var out []fileChange
	seen := make(map[int64]bool)
	for tip != 0 {
		if seen[tip] {
			return nil, errors.New("cyclic file change history")
		}
		seen[tip] = true
		var c fileChange
		var data string
		if err := s.db.QueryRow("SELECT id,coalesce(previous_id,0),reversible,paths_json FROM file_changes WHERE id=?", tip).Scan(&c.id, &c.previous, &c.reversible, &data); err != nil {
			return nil, err
		}
		c.paths = json.RawMessage(data)
		out = append(out, c)
		tip = c.previous
	}
	return out, nil
}

func (s *Store) validateRestore(plan RestorePlan) error {
	if plan.store != s || plan.session == "" {
		return errors.New("restore plan was not issued by this history store")
	}
	v, err := s.Session(plan.session)
	if err != nil {
		return err
	}
	if v.ReadOnly {
		return errors.New("session is read-only")
	}
	if (restoreCursor{v.EntryTip, v.FileTip, v.RedoTip}) != plan.before {
		return errors.New("history changed since restore was planned")
	}
	return nil
}

// RestoreChanges resolves the file-history divergence into ordered undo/forward
// payloads, excluding the shared prefix. It rejects stale plans and irreversible
// changes before returning any effects. It never reads or writes workspace files.
func (s *Store) RestoreChanges(plan RestorePlan) ([]RestoreChange, error) {
	if err := s.validateRestore(plan); err != nil {
		return nil, err
	}
	old, err := s.fileChanges(plan.before.file)
	if err != nil {
		return nil, err
	}
	next, err := s.fileChanges(plan.after.file)
	if err != nil {
		return nil, err
	}
	ancestors := map[int64]bool{0: true}
	for _, c := range next {
		ancestors[c.id] = true
	}
	var common int64
	for _, c := range old {
		if ancestors[c.id] {
			common = c.id
			break
		}
	}
	var changes []RestoreChange
	add := func(c fileChange, reverse bool) error {
		if !c.reversible {
			return errors.New("restoration crosses a non-undoable outside-workspace edit")
		}
		changes = append(changes, RestoreChange{Paths: c.paths, Reverse: reverse})
		return nil
	}
	for _, c := range old {
		if c.id == common {
			break
		}
		if err := add(c, true); err != nil {
			return nil, err
		}
	}
	var suffix []fileChange
	for _, c := range next {
		if c.id == common {
			break
		}
		suffix = append(suffix, c)
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		if err := add(suffix[i], false); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

// CommitRestore publishes the plan's conversation/file selection only after the
// caller has completely applied its filesystem effects. Stale, foreign and
// read-only plans fail without changing history. Files are not repaired.
func (s *Store) CommitRestore(plan RestorePlan) error {
	if plan.store != s || plan.session == "" {
		return errors.New("restore plan was not issued by this history store")
	}
	return s.transact(func(tx *sql.Tx) error {
		r, err := tx.Exec(`UPDATE sessions SET active_entry_id=?,file_tip_id=?,redo_entry_id=?,last_activity_ms=?
			WHERE id=? AND read_only=0 AND coalesce(active_entry_id,0)=? AND coalesce(file_tip_id,0)=? AND coalesce(redo_entry_id,0)=?`,
			n(plan.after.entry), n(plan.after.file), n(plan.after.redo), time.Now().UnixMilli(), plan.session,
			plan.before.entry, plan.before.file, plan.before.redo)
		if err != nil {
			return err
		}
		count, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("history changed since restore was planned or session is read-only")
		}
		return nil
	})
}
