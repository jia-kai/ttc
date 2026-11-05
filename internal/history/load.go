package history

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"ttc/internal/llm"
)

// Load snapshots a writable conversation into an independent session, with an
// undo boundary at the imported tip and no file changes or restored live work.
// It excludes the suffix after the last balanced main tool exchange. Source
// entries, requests and turns remain unchanged. Forks share a frozen prefix and
// their lineage's immutable assets and retention lifetime. At a compaction boundary
// it atomically recovers durable pending events into fresh fork-owned rows.
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
		entries, err := forkScanWith(tx, id, 0)
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
			boundary, err = forkBoundaryWith(tx, entries)
			if err != nil {
				return err
			}
			if boundary {
				ancestry, err := forkRecoveryAncestryWith(tx, id, entries)
				if err != nil {
					return err
				}
				if err := forkRecoveryEntriesWith(tx, ancestry); err != nil {
					return err
				}
				recovery, err = recoveryCandidatesWith(tx, id, ancestry, entries[:end])
				if err != nil {
					return err
				}
			}
		}
		var base int64
		if end > 0 {
			base = entries[end-1].ID
		}
		loaded = NewID("session")
		_, err = tx.Exec(`INSERT INTO sessions(id,workspace_id,lineage_id,name,name_source,model_json,last_activity_ms,metadata_json,base_session_id,base_entry_id,active_entry_id)
			SELECT ?,workspace_id,lineage_id,name,'manual',model_json,?,
			CASE WHEN json_extract(metadata_json,'$.compaction_error') IS NULL THEN '{}'
			ELSE json_object('compaction_error',json_extract(metadata_json,'$.compaction_error')) END,id,?,?
			FROM sessions WHERE id=?`, loaded, time.Now().UnixMilli(), n(base), n(base), id)
		if err != nil {
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
