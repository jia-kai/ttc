package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"ttc/internal/prompts"
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
			return errors.New(prompts.HistorySessionReadOnly)
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
