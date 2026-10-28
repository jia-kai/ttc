package history

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// Load snapshots a writable conversation into an independent session, with an
// undo boundary at the imported tip and no file changes or restored live work.
// It excludes the suffix after the last balanced main tool exchange. Source
// entries, requests and turns remain unchanged. Copies share their lineage's
// immutable assets and retention lifetime. At a compaction boundary it atomically
// recovers durable pending events into fresh snapshot-owned notification rows.
// Read-only predecessors open for inspection.
func (s *Store) Load(id string) (Session, error) {
	loaded := id
	err := s.transact(func(tx *sql.Tx) error {
		var readOnly bool
		var fatal string
		if err := tx.QueryRow("SELECT read_only,coalesce(json_extract(metadata_json,'$.compaction_error'),'') FROM sessions WHERE id=?", id).Scan(&readOnly, &fatal); err != nil {
			return err
		}
		if readOnly {
			return nil
		}
		entries, err := branchWith(tx, id, 0, false)
		if err != nil {
			return err
		}
		pending := map[string]bool{}
		end := 0
		for i, entry := range entries {
			if entry.Visible && entry.Role == "assistant" {
				var message llm.Message
				if err := json.Unmarshal(entry.Content, &message); err != nil {
					return err
				}
				for _, call := range message.Calls {
					if call.ID == "" || pending[call.ID] {
						return fmt.Errorf("invalid tool exchange at entry %d", entry.ID)
					}
					pending[call.ID] = true
				}
			} else if entry.Visible && entry.Kind == "tool_result" {
				var ref struct {
					CallID string `json:"call_id"`
				}
				if err := json.Unmarshal(entry.Content, &ref); err != nil {
					return err
				}
				var providerID string
				if err := tx.QueryRow("SELECT provider_call_id FROM tool_calls WHERE id=?", ref.CallID).Scan(&providerID); err != nil {
					return err
				}
				if !pending[providerID] {
					return fmt.Errorf("unpaired tool result at entry %d", entry.ID)
				}
				delete(pending, providerID)
			}
			if len(pending) == 0 {
				end = i + 1
			}
		}
		var recovery []recoveryEvent
		boundary := false
		if fatal == "" {
			boundary, err = compactionBoundaryWith(tx, entries)
			if err != nil {
				return err
			}
			if boundary {
				ancestry, err := recoveryAncestryWith(tx, id, entries)
				if err != nil {
					return err
				}
				recovery, err = recoveryCandidatesWith(tx, ancestry, entries[:end])
				if err != nil {
					return err
				}
			}
		}
		loaded = NewID("session")
		_, err = tx.Exec(`INSERT INTO sessions(id,workspace_id,lineage_id,name,name_source,model_json,last_activity_ms,metadata_json)
			SELECT ?,workspace_id,lineage_id,name,'manual',model_json,?,
			CASE WHEN json_extract(metadata_json,'$.compaction_error') IS NULL THEN '{}'
			ELSE json_object('compaction_error',json_extract(metadata_json,'$.compaction_error')) END
			FROM sessions WHERE id=?`, loaded, time.Now().UnixMilli(), id)
		if err != nil {
			return err
		}
		if err = copyLoadedEntries(tx, loaded, entries[:end]); err != nil {
			return err
		}
		for _, event := range recovery {
			data, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if _, err = appendTx(tx, loaded, "", "main", "status", "", false, data, 0); err != nil {
				return err
			}
		}
		if boundary {
			if _, err = appendTx(tx, loaded, "", "main", "status", "", false, json.RawMessage(`{"type":"compaction_recovery"}`), 0); err != nil {
				return err
			}
		}
		_, err = tx.Exec("UPDATE sessions SET undo_floor_id=active_entry_id WHERE id=?", loaded)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return s.Session(loaded)
}

// copyLoadedEntries imports a frozen balanced prefix. Keep payload copies in
// SQLite and advance the target cursor once, not once per historical entry.
// Imported rows have no file checkpoints or delivery acknowledgments; source
// ownership and original timestamps remain immutable historical evidence.
func copyLoadedEntries(tx *sql.Tx, session string, entries []Entry) error {
	copyEntry, err := tx.Prepare(`INSERT INTO entries(session_id,parent_id,source_id,turn_id,main_turn_id,undo_owner_turn_id,actor_id,kind,role,model_visible,content_json,created_ms)
		SELECT ?,?,coalesce(e.source_id,e.id),e.turn_id,origin.main_turn_id,origin.undo_owner_turn_id,e.actor_id,e.kind,e.role,?,e.content_json,e.created_ms
		FROM entries e JOIN entries origin ON origin.id=coalesce(e.source_id,e.id) WHERE e.id=?`)
	if err != nil {
		return err
	}
	defer copyEntry.Close()
	copyRecord, err := tx.Prepare(`INSERT INTO tool_records(entry_id,call_id,version,record_json,markdown_json)
		SELECT ?,call_id,version,record_json,markdown_json FROM tool_records WHERE entry_id=?`)
	if err != nil {
		return err
	}
	defer copyRecord.Close()
	var parent int64
	for _, entry := range entries {
		visible := entry.Visible
		if entry.Kind == "message" || entry.Kind == "summary" {
			var message llm.Message
			if err := json.Unmarshal(entry.Content, &message); err != nil {
				return fmt.Errorf(prompts.HistoryDecodeCanonicalMessage, entry.Kind, err)
			}
			if message.Role() != entry.Role {
				return fmt.Errorf(prompts.HistoryMessageIndexedRole, message.Type, message.Role(), entry.Role)
			}
			if entry.Kind == "message" && message.Type.IsRuntime() {
				visible = false
			}
		}
		result, err := copyEntry.Exec(session, n(parent), visible, entry.ID)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil {
			return err
		} else if count != 1 {
			return fmt.Errorf("missing source for loaded entry %d", entry.ID)
		}
		parent, err = result.LastInsertId()
		if err != nil {
			return err
		}
		if entry.Kind == "tool_result" {
			if _, err := copyRecord.Exec(parent, entry.ID); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec("UPDATE sessions SET active_entry_id=? WHERE id=?", n(parent), session)
	return err
}
