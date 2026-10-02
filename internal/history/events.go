package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"scicode/internal/provider"
)

type historyReader interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// EventSeq returns the immutable semantic commit identity. Continuation copies
// preserve their original source; physical row IDs remain separate.
func (e Entry) EventSeq() int64 {
	if e.Source != 0 {
		return e.Source
	}
	return e.ID
}

// Admission contains one immutable committed coding request and its input.
// Cutoff bounds source events; ContextEntry is zero when no snapshot was added.
// NoticeEntries are presentation-only delivery rows.
type Admission struct {
	RequestID, ContextEntry, Cutoff int64
	NoticeEntries                   []int64
	SteerEntries                    []int64
	Messages                        []provider.Message
}

// requestInputMetadata describes an admitted input without duplicating its
// messages. Exact records remain in history; retries own the frozen input in memory.
type requestInputMetadata struct {
	Version      int `json:"version"`
	MessageCount int `json:"message_count"`
}

// AdmitRequest atomically snapshots main input, records an optional new runtime
// context and notification acknowledgments, and admits a request before network I/O.
// Messages contains the admitted main projection or the caller's isolated child
// input, with any new runtime context appended.
func (s *Store) AdmitRequest(session, turn, actor string, model provider.Selection, contextMessage *provider.Message, notices, childInput []provider.Message, steers ...provider.Message) (v Admission, err error) {
	if contextMessage != nil && (contextMessage.Role != "developer" || !contextMessage.Runtime || contextMessage.Content == "") {
		return v, errors.New("runtime context must be a nonempty developer runtime message")
	}
	data, err := json.Marshal(model)
	if err != nil {
		return v, err
	}
	err = s.transact(func(tx *sql.Tx) error {
		var invalid string
		if err := tx.QueryRow("SELECT coalesce(json_extract(metadata_json,'$.compaction_error'),'') FROM sessions WHERE id=?", session).Scan(&invalid); err != nil {
			return err
		}
		if invalid != "" {
			return fmt.Errorf("session unusable after compaction: %s; start or load another session", invalid)
		}
		if err := tx.QueryRow("SELECT coalesce(max(id),0) FROM entries").Scan(&v.Cutoff); err != nil {
			return err
		}
		delivered := make([]int64, 0, len(notices))
		for _, m := range notices {
			if m.EventSeq <= 0 || m.EventSeq > v.Cutoff {
				return errors.New("notification is not a committed event at request cutoff")
			}
			var source, deliveredRequest sql.NullInt64
			if err := tx.QueryRow("SELECT e.source_id,e.delivered_request_id FROM entries e JOIN sessions original ON original.id=e.session_id JOIN sessions active ON active.id=? WHERE e.id=? AND original.lineage_id=active.lineage_id", session, m.EventSeq).Scan(&source, &deliveredRequest); err != nil {
				return fmt.Errorf("notification source: %w", err)
			}
			if source.Valid || deliveredRequest.Valid {
				return errors.New("notification source is copied or already delivered")
			}
			delivered = append(delivered, m.EventSeq)
			m.Runtime = true
			b, err := json.Marshal(m)
			if err != nil {
				return err
			}
			id, err := appendTx(tx, session, turn, "main", "message", "user", true, b, 0)
			if err != nil {
				return err
			}
			v.NoticeEntries = append(v.NoticeEntries, id)
		}
		if actor != "main" && len(steers) > 0 {
			return errors.New("only main requests accept human steering")
		}
		for _, m := range steers {
			if m.Role != "user" || m.Runtime {
				return errors.New("steering must be a human user message")
			}
			checkpoint := NewID("steer")
			now := time.Now().UnixMilli()
			_, err := tx.Exec(`INSERT INTO turns(id,session_id,actor_id,trigger,start_entry_id,start_file_tip_id,status,model_json,started_ms,finished_ms) SELECT ?,id,'main','steer',active_entry_id,file_tip_id,'completed',?,?,? FROM sessions WHERE id=?`, checkpoint, string(data), now, now, session)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE sessions SET metadata_json=json_set(metadata_json,'$.undo_owner_turn_id',?) WHERE id=?", checkpoint, session); err != nil {
				return err
			}
			b, err := json.Marshal(m)
			if err != nil {
				return err
			}
			id, err := appendTx(tx, session, checkpoint, "main", "message", "user", true, b, 0)
			if err != nil {
				return err
			}
			v.SteerEntries = append(v.SteerEntries, id)
		}
		ack, err := json.Marshal(delivered)
		if err != nil {
			return err
		}
		r, err := tx.Exec(`INSERT INTO model_requests(session_id,turn_id,actor_id,purpose,model_json,status,attempts_json,event_cutoff,delivered_events_json,created_ms) VALUES(?,?,?,'coding',?,'running','[]',?,?,?)`, session, textOrNil(turn), actor, string(data), v.Cutoff, string(ack), time.Now().UnixMilli())
		if err != nil {
			return err
		}
		v.RequestID, err = r.LastInsertId()
		if err != nil {
			return err
		}
		if contextMessage != nil {
			message := *contextMessage
			message.RequestID = v.RequestID
			b, err := json.Marshal(message)
			if err != nil {
				return err
			}
			v.ContextEntry, err = appendTx(tx, session, turn, actor, "message", "developer", actor == "main", b, 0)
			if err != nil {
				return err
			}
		}
		_, err = appendTx(tx, session, turn, actor, "status", "", false, json.RawMessage(fmt.Sprintf(`{"type":"request_admitted","request_id":%d,"event_cutoff":%d}`, v.RequestID, v.Cutoff)), 0)
		if err != nil {
			return err
		}
		if actor == "main" {
			v.Messages, err = messagesWith(tx, session)
		} else {
			v.Messages = append([]provider.Message(nil), childInput...)
			if contextMessage != nil {
				message := *contextMessage
				message.RequestID = v.RequestID
				v.Messages = append(v.Messages, message)
			}
		}
		if err != nil {
			return err
		}
		v.Messages = provider.ContextFor(model, v.Messages)
		// Foreground children carry their finish event in an immutable tool
		// result rather than producing an extra notification message.
		for _, m := range v.Messages {
			if m.Role != "tool" {
				continue
			}
			var result struct {
				Finish int64 `json:"finish_event_seq"`
			}
			if json.Unmarshal([]byte(m.Content), &result) != nil || result.Finish == 0 {
				continue
			}
			if result.Finish > v.Cutoff {
				return errors.New("child finish exceeds request cutoff")
			}
			delivered = append(delivered, result.Finish)
		}
		// A source event is delivered once per lineage, including foreground
		// results still present in later request history. Copies retain source ID.
		first := make([]int64, 0, len(delivered))
		seen := map[int64]bool{}
		for _, event := range delivered {
			if seen[event] {
				continue
			}
			seen[event] = true
			result, err := tx.Exec("UPDATE entries SET delivered_request_id=? WHERE id=? AND source_id IS NULL AND delivered_request_id IS NULL AND session_id IN (SELECT id FROM sessions WHERE lineage_id=(SELECT lineage_id FROM sessions WHERE id=?))", v.RequestID, event, session)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count == 1 {
				first = append(first, event)
			}
		}
		ack, err = json.Marshal(first)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE model_requests SET delivered_events_json=? WHERE id=?", string(ack), v.RequestID); err != nil {
			return err
		}
		input, err := json.Marshal(requestInputMetadata{Version: 1, MessageCount: len(v.Messages)})
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE model_requests SET input_json=? WHERE id=?", string(input), v.RequestID)
		return err
	})
	return v, err
}

// BeginChildTurn records an isolated assignment without changing main turn or
// human undo ownership. The caller serializes follow-up admission per child.
func (s *Store) BeginChildTurn(session, actor string, model provider.Selection) (string, error) {
	id := NewID("ct")
	b, err := json.Marshal(model)
	if err != nil {
		return "", err
	}
	err = s.transact(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO turns(id,session_id,actor_id,trigger,status,model_json,started_ms) VALUES(?,?,?,'child','running',?,?)`, id, session, actor, string(b), time.Now().UnixMilli())
		return err
	})
	return id, err
}

// InvalidateContext permanently disables inference in a main context without
// deleting its inspectable history. Reloading does not clear the failure.
func (s *Store) InvalidateContext(session, reason string) error {
	if reason == "" {
		return errors.New("missing compaction failure reason")
	}
	return s.transact(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE sessions SET metadata_json=json_set(metadata_json,'$.compaction_error',?) WHERE id=?", reason, session)
		if err != nil {
			return err
		}
		b, err := json.Marshal(map[string]string{"type": "compaction_failed", "error": reason, "text": "Context unusable after compaction: " + reason})
		if err != nil {
			return err
		}
		_, err = appendTx(tx, session, "", "main", "status", "", false, b, 0)
		return err
	})
}
