package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CommitChange records an applied subset and advances its session file tip atomically.
func (s *Store) CommitChange(session, call string, paths any, reversible bool) (int64, error) {
	b, e := json.Marshal(paths)
	if e != nil {
		return 0, e
	}
	var id int64
	e = s.transact(func(tx *sql.Tx) error {
		var ro bool
		if e := tx.QueryRow("SELECT read_only FROM sessions WHERE id=?", session).Scan(&ro); e != nil {
			return e
		}
		if ro {
			return errors.New("session is read-only")
		}
		r, e := tx.Exec(`INSERT INTO file_changes(session_id,previous_id,call_id,paths_json,reversible,created_ms) SELECT id,file_tip_id,?,?,?,? FROM sessions WHERE id=?`, call, string(b), reversible, time.Now().UnixMilli(), session)
		if e != nil {
			return e
		}
		id, e = r.LastInsertId()
		if e != nil {
			return e
		}
		if _, e = tx.Exec("UPDATE sessions SET file_tip_id=? WHERE id=?", id, session); e != nil {
			return e
		}
		var actor, turn string
		if e := tx.QueryRow("SELECT c.actor_id,coalesce(q.turn_id,'') FROM tool_calls c JOIN model_requests q ON q.id=c.request_id WHERE c.id=?", call).Scan(&actor, &turn); e != nil {
			return e
		}
		event, _ := json.Marshal(map[string]any{"type": "file_changed", "change_id": id, "call_id": call, "paths": paths})
		if _, e := appendTx(tx, session, turn, actor, "status", "", false, event, 0); e != nil {
			return e
		}
		return nil
	})
	return id, e
}

// Change is an immutable link in shared file history.
type Change struct {
	ID, Previous int64
	Reversible   bool
	Paths        json.RawMessage
}

// Changes walks a file tip backward; it includes predecessor-session shared links.
func (s *Store) Changes(tip int64) ([]Change, error) {
	var out []Change
	for tip != 0 {
		var c Change
		var data string
		if e := s.DB.QueryRow("SELECT id,coalesce(previous_id,0),reversible,paths_json FROM file_changes WHERE id=?", tip).Scan(&c.ID, &c.Previous, &c.Reversible, &data); e != nil {
			return nil, e
		}
		c.Paths = json.RawMessage(data)
		out = append(out, c)
		tip = c.Previous
	}
	return out, nil
}

// RestoreTarget describes cursor and file tip changes that accompany restoration.
type RestoreTarget struct {
	EntryTip int64 `json:"entry_tip"`
	FileTip  int64 `json:"file_tip"`
	RedoTip  int64 `json:"redo_tip"`
}

// UndoTarget finds the newest selected user turn and enforces the session undo floor.
func (s *Store) UndoTarget(session string) (RestoreTarget, error) {
	v, e := s.Session(session)
	if e != nil {
		return RestoreTarget{}, e
	}
	if v.ReadOnly {
		return RestoreTarget{}, errors.New("session is read-only")
	}
	entries, e := s.Branch(session, 0)
	if e != nil {
		return RestoreTarget{}, e
	}
	for i := len(entries) - 1; i >= 0; i-- {
		x := entries[i]
		if x.ID <= v.UndoFloor {
			break
		}
		if x.Kind == "message" && x.Actor == "main" && x.Role == "user" && x.TurnID != "" {
			var start, tip sql.NullInt64
			var trigger string
			if e = s.DB.QueryRow("SELECT start_entry_id,start_file_tip_id,trigger FROM turns WHERE id=?", x.TurnID).Scan(&start, &tip, &trigger); e != nil {
				return RestoreTarget{}, e
			}
			if trigger != "user" && trigger != "steer" {
				continue
			}
			if start.Int64 < v.UndoFloor {
				return RestoreTarget{}, errors.New("undo would cross session boundary")
			}
			return RestoreTarget{start.Int64, tip.Int64, v.EntryTip}, nil
		}
	}
	return RestoreTarget{}, errors.New("nothing to undo")
}

// BranchTarget resolves a selected node in the active session; no cross-session restore.
func (s *Store) BranchTarget(session string, id int64) (RestoreTarget, error) {
	v, e := s.Session(session)
	if e != nil {
		return RestoreTarget{}, e
	}
	if v.ReadOnly {
		return RestoreTarget{}, errors.New("session is read-only")
	}
	if id < v.UndoFloor {
		return RestoreTarget{}, errors.New("branch crosses undo boundary")
	}
	if id == 0 {
		return RestoreTarget{}, nil
	}
	var tip int64
	if e = s.DB.QueryRow("SELECT coalesce(file_tip_id,0) FROM entries WHERE id=? AND session_id=?", id, session).Scan(&tip); e != nil {
		return RestoreTarget{}, fmt.Errorf("branch target: %w", e)
	}
	return RestoreTarget{id, tip, 0}, nil
}

// CommitRestore advances both cursors only after complete filesystem restoration.
func (s *Store) CommitRestore(session string, target RestoreTarget) error {
	return s.transact(func(tx *sql.Tx) error {
		if _, e := tx.Exec(`UPDATE sessions SET active_entry_id=?,file_tip_id=?,redo_entry_id=?,last_activity_ms=? WHERE id=?`, n(target.EntryTip), n(target.FileTip), n(target.RedoTip), time.Now().UnixMilli(), session); e != nil {
			return e
		}
		return nil
	})
}
