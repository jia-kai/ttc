package history

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"scicode/internal/provider"
)

// Load snapshots a writable conversation into an independent session, with an
// undo boundary at the imported tip and no file changes or restored live work.
// It excludes the suffix after the last balanced main tool exchange. Source
// entries, requests and turns remain unchanged. Copies share their lineage's
// immutable assets and retention lifetime. Read-only predecessors open for inspection.
func (s *Store) Load(id string) (Session, error) {
	if err := s.ValidateArchive(id); err != nil {
		return Session{}, err
	}
	loaded := id
	err := s.transact(func(tx *sql.Tx) error {
		var readOnly bool
		if err := tx.QueryRow("SELECT read_only FROM sessions WHERE id=?", id).Scan(&readOnly); err != nil {
			return err
		}
		if readOnly {
			return nil
		}
		entries, err := branchWith(tx, id, 0)
		if err != nil {
			return err
		}
		pending := map[string]bool{}
		end := 0
		for i, entry := range entries {
			if entry.Visible && entry.Role == "assistant" {
				var message provider.Message
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
		loaded = NewID("session")
		_, err = tx.Exec(`INSERT INTO sessions(id,workspace_id,lineage_id,name,name_source,model_json,last_activity_ms,metadata_json)
			SELECT ?,workspace_id,lineage_id,name,'manual',model_json,?,
			CASE WHEN json_extract(metadata_json,'$.compaction_error') IS NULL THEN '{}'
			ELSE json_object('compaction_error',json_extract(metadata_json,'$.compaction_error')) END
			FROM sessions WHERE id=?`, loaded, time.Now().UnixMilli(), id)
		if err != nil {
			return err
		}
		for _, entry := range entries[:end] {
			source := entry.EventSeq()
			visible := entry.Visible
			if visible && entry.Kind == "message" {
				var message provider.Message
				if err := json.Unmarshal(entry.Content, &message); err != nil {
					return err
				}
				visible = !message.Runtime // Live environment/notifications belong to the new runtime.
			}
			copyID, err := appendTx(tx, loaded, entry.TurnID, entry.Actor, entry.Kind, entry.Role, visible, entry.Content, source)
			if err != nil {
				return err
			}
			// Copied chronology is historical context, never a new execution or file checkpoint.
			if _, err = tx.Exec("UPDATE entries SET created_ms=? WHERE id=?", entry.CreatedMS, copyID); err != nil {
				return err
			}
			if entry.Kind == "tool_result" {
				if _, err = tx.Exec("INSERT INTO tool_records(entry_id,call_id,version,record_json,markdown_json) SELECT ?,call_id,version,record_json,markdown_json FROM tool_records WHERE entry_id=?", copyID, entry.ID); err != nil {
					return err
				}
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
