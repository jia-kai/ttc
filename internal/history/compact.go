package history

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	ctxmgr "scicode/internal/context"
	"scicode/internal/provider"
	"syscall"
	"time"
)

// Continue atomically freezes a predecessor and copies a retained visible suffix.
// Callers pause main model work and serialize this handoff against child/file commits.
// inputIDs selects sorted, unique visible main human entries strictly before
// retainFrom: at most two normal/queued inputs combined and two steers. Each copy
// follows a source/age marker frozen at compactedAt, with its file tip and undo
// checkpoint rebased to the tip immediately before retainFrom. retainFrom must
// identify an active-branch entry, or the final entry ID plus one for an empty suffix.
func (s *Store) Continue(session, summary, archive string, retainFrom int64, inputIDs []int64, compactedAt time.Time) (Session, error) {
	archiveHash, e := filepathHash(archive)
	if e != nil {
		return Session{}, fmt.Errorf("validate Markdown compaction archive: %w", e)
	}
	exactHash, e := filepathHash(archive + ".jsonl")
	if e != nil {
		return Session{}, fmt.Errorf("validate exact compaction archive: %w", e)
	}
	old, e := s.Session(session)
	if e != nil {
		return Session{}, e
	}
	entries, e := s.Branch(session, 0)
	if e != nil {
		return Session{}, e
	}
	if old.ReadOnly || summary == "" || len(entries) == 0 {
		return Session{}, errors.New("invalid continuation")
	}
	if retainFrom <= 0 || compactedAt.IsZero() || len(inputIDs) > 4 {
		return Session{}, errors.New("invalid continuation retention boundary")
	}
	selected := make(map[int64]bool, len(inputIDs))
	for i, input := range inputIDs {
		if input <= 0 || input >= retainFrom || (i > 0 && input <= inputIDs[i-1]) {
			return Session{}, errors.New("continuation inputs must be sorted unique IDs before the suffix")
		}
		selected[input] = true
	}
	id := NewID("session")
	e = s.transact(func(tx *sql.Tx) error {
		metadata, e := inputMetadataWith(tx, entries)
		if e != nil {
			return e
		}
		inputs := make(map[int64]provider.Message, len(inputIDs))
		markers := make(map[int64]provider.Message, len(inputIDs))
		boundaryFound := retainFrom == entries[len(entries)-1].ID+1
		normal, steers := 0, 0
		for _, entry := range entries {
			boundaryFound = boundaryFound || entry.ID == retainFrom
			if !selected[entry.ID] {
				continue
			}
			var message provider.Message
			if e := json.Unmarshal(entry.Content, &message); e != nil {
				return e
			}
			if !entry.Visible || entry.Kind != "message" || entry.Role != "user" || entry.Actor != "main" || message.Role != "user" || message.Runtime {
				return errors.New("continuation input must identify a visible main human message")
			}
			message, e = enrichInput(metadata, entry, message)
			if e != nil {
				return e
			}
			if message.InputSource == "steer" {
				steers++
			} else {
				normal++
			}
			marker, e := ctxmgr.InputMarker(message, compactedAt)
			if e != nil {
				return e
			}
			inputs[entry.ID], markers[entry.ID] = message, marker
		}
		if !boundaryFound || len(inputs) != len(inputIDs) || normal > 2 || steers > 2 {
			return errors.New("invalid continuation input selection or suffix boundary")
		}
		var ordinal int
		if e := tx.QueryRow("SELECT count(*) FROM compactions c JOIN sessions s ON s.id=c.continuation_id WHERE s.lineage_id=?", old.LineageID).Scan(&ordinal); e != nil {
			return e
		}
		var rootName string
		if e := tx.QueryRow("SELECT name FROM sessions WHERE id=?", old.LineageID).Scan(&rootName); e != nil {
			return e
		}
		if _, e := tx.Exec("UPDATE sessions SET read_only=1 WHERE id=? AND read_only=0", session); e != nil {
			return e
		}
		m, _ := json.Marshal(old.Model)
		_, e = tx.Exec(`INSERT INTO sessions(id,workspace_id,lineage_id,predecessor_id,name,name_source,model_json,observed_generation,last_activity_ms,metadata_json) SELECT ?,workspace_id,lineage_id,id,?,'continuation',?,observed_generation,last_activity_ms,metadata_json FROM sessions WHERE id=?`, id, fmt.Sprintf("%s-cont-%d", rootName, ordinal), string(m), session)
		if e != nil {
			return e
		}
		baseline := int64(0)
		for _, entry := range entries {
			if entry.ID >= retainFrom {
				break
			}
			baseline = entry.FileTip
		}
		if _, e = tx.Exec("UPDATE sessions SET file_tip_id=? WHERE id=?", n(baseline), id); e != nil {
			return e
		}
		data, _ := json.Marshal(provider.Message{Role: "assistant", Content: summary})
		summaryID, e := appendTx(tx, id, "", "main", "summary", "assistant", true, data, 0)
		if e != nil {
			return e
		}
		mapping := map[int64]int64{}
		for _, entry := range entries {
			if entry.ID < retainFrom && !selected[entry.ID] {
				continue
			}
			fileTip := entry.FileTip
			if selected[entry.ID] {
				fileTip = baseline
			}
			if _, e = tx.Exec("UPDATE sessions SET file_tip_id=? WHERE id=?", n(fileTip), id); e != nil {
				return e
			}
			source := entry.Source
			if source == 0 {
				source = entry.ID
			}
			content := entry.Content
			if marker, ok := markers[entry.ID]; ok {
				data, e := json.Marshal(marker)
				if e != nil {
					return e
				}
				if _, e = appendTx(tx, id, "", "main", "message", "developer", true, data, 0); e != nil {
					return e
				}
			}
			if entry.Visible && (entry.Kind == "message" || entry.Kind == "summary") {
				var m provider.Message
				if input, ok := inputs[entry.ID]; ok {
					m = input
				} else {
					if e = json.Unmarshal(content, &m); e != nil {
						return e
					}
					m, e = enrichInput(metadata, entry, m)
					if e != nil {
						return e
					}
				}
				m.State = nil
				content, e = json.Marshal(m)
				if e != nil {
					return e
				}
			}
			newid, e := appendTx(tx, id, entry.TurnID, entry.Actor, entry.Kind, entry.Role, entry.Visible, content, source)
			if e != nil {
				return e
			}
			mapping[entry.ID] = newid
			if entry.Kind == "tool_result" {
				_, e = tx.Exec("INSERT INTO tool_records(entry_id,call_id,version,record_json,markdown_json) SELECT ?,call_id,version,record_json,markdown_json FROM tool_records WHERE entry_id=?", newid, entry.ID)
				if e != nil {
					return e
				}
			}
		}
		if _, e = tx.Exec("UPDATE sessions SET file_tip_id=?,undo_floor_id=? WHERE id=?", n(old.FileTip), summaryID, id); e != nil {
			return e
		}
		retainedTurns := map[string]bool{}
		for _, entry := range entries {
			if (entry.ID >= retainFrom || selected[entry.ID]) && entry.TurnID != "" {
				retainedTurns[entry.TurnID] = true
			}
		}
		for turn := range retainedTurns {
			var start, tip int64
			if e := tx.QueryRow("SELECT coalesce(start_entry_id,0),coalesce(start_file_tip_id,0) FROM turns WHERE id=?", turn).Scan(&start, &tip); e != nil {
				return e
			}
			checkpoint, ok := mapping[start]
			if !ok {
				checkpoint = summaryID
			}
			if !ok || selected[start] {
				tip = baseline
			}
			if _, e = tx.Exec("UPDATE turns SET session_id=?,start_entry_id=?,start_file_tip_id=? WHERE id=?", id, checkpoint, n(tip), turn); e != nil {
				return e
			}
		}
		// In-flight actors can have their entire visible tail summarized. Their
		// active turn still routes to the writable continuation before finishing.
		if _, e = tx.Exec("UPDATE turns SET session_id=? WHERE session_id=? AND status='running'", id, session); e != nil {
			return e
		}
		_, e = tx.Exec("INSERT INTO compactions(continuation_id,predecessor_id,source_tip_id,ordinal,archive_path,archive_sha256,archive_exact_sha256,summary_entry_id) VALUES(?,?,?,?,?,?,?,?)", id, session, old.EntryTip, ordinal, archive, archiveHash, exactHash, summaryID)
		return e
	})
	if e != nil {
		return Session{}, e
	}
	return s.Session(id)
}

func filepathHash(path string) (string, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return "", e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return "", e
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("compaction archive must be a regular file")
	}
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
