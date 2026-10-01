package history

import (
	"database/sql"
	"encoding/json"
	"scicode/internal/render"
)

// RecoverCalls closes every uncertain call without executing any saved operation.
func (s *Store) RecoverCalls() error {
	return s.transact(func(tx *sql.Tx) error {
		rows, e := tx.Query(`SELECT c.id,c.session_id,c.actor_id,c.name,c.call_json,r.turn_id FROM tool_calls c JOIN model_requests r ON r.id=c.request_id WHERE c.result_json IS NULL`)
		if e != nil {
			return e
		}
		type pending struct {
			id, session, actor, name, args string
			turn                           sql.NullString
		}
		var calls []pending
		for rows.Next() {
			var p pending
			if e = rows.Scan(&p.id, &p.session, &p.actor, &p.name, &p.args, &p.turn); e != nil {
				rows.Close()
				return e
			}
			calls = append(calls, p)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, p := range calls {
			result := json.RawMessage(`{"ok":false,"error":{"code":"interrupted","message":"runtime ended before result was committed"}}`)
			if _, e = tx.Exec("UPDATE tool_calls SET result_json=? WHERE id=?", string(result), p.id); e != nil {
				return e
			} // An unfinished child call may have started in a now read-only predecessor.
			var session string
			if e = tx.QueryRow(`SELECT writable.id FROM sessions origin JOIN sessions writable ON writable.lineage_id=origin.lineage_id AND writable.read_only=0 WHERE origin.id=?`, p.session).Scan(&session); e != nil {
				return e
			}
			ref, _ := json.Marshal(map[string]string{"call_id": p.id})
			entry, e := appendTx(tx, session, p.turn.String, p.actor, "tool_result", "tool", p.actor == "main", ref, 0)
			if e != nil {
				return e
			}
			md := render.Tool(p.name, []byte(p.args), result)
			markdown, _ := json.Marshal(md)
			record, _ := json.Marshal(map[string]any{"name": p.name, "arguments": json.RawMessage(p.args), "result": result, "markdown": md})
			if _, e = tx.Exec("INSERT INTO tool_records(entry_id,call_id,version,record_json,markdown_json) VALUES(?,?,1,?,?)", entry, p.id, string(record), string(markdown)); e != nil {
				return e
			}
		}
		return nil
	})
}
