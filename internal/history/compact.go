package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	ctxmgr "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// Continue atomically freezes a predecessor and copies a retained visible suffix.
// handoff is the exact checked compaction context, persisted without reframing.
// sourceTip is the original selected cut summarized before inference, not the
// predecessor tip at handoff; actor commits after that cut remain copied tails.
// Hidden main compaction request-message bodies stay in the predecessor; small
// lifecycle and instruction metadata remains in the copied tail.
// Callers pause main model work and serialize this handoff against child/file commits.
// inputIDs selects sorted, unique visible main human entries strictly before
// retainFrom: at most two normal/queued inputs combined and two steers. Each copy
// carries its retention time frozen at compactedAt, with its file tip and undo
// checkpoint rebased to the tip immediately before retainFrom. retainFrom must
// identify an active-branch entry, or the final entry ID plus one for an empty suffix.
// pending preserves an already-persisted runtime recovery warning, with its
// original request/source identity, if it is not in the copied visible suffix.
func (s *Store) Continue(session string, handoff llm.Message, sourceTip, retainFrom int64, inputIDs []int64, compactedAt time.Time, pending *llm.Message) (Session, error) {
	if handoff.Type != llm.TypeCompactionContext || handoff.Content == "" {
		return Session{}, errors.New(prompts.HistoryContinuation)
	}
	if e := handoff.Validate(); e != nil {
		return Session{}, e
	}
	data, e := json.Marshal(handoff)
	if e != nil {
		return Session{}, e
	}
	old, e := s.Session(session)
	if e != nil {
		return Session{}, e
	}
	entries, e := s.Branch(session, 0)
	if e != nil {
		return Session{}, e
	}
	if old.ReadOnly || len(entries) == 0 {
		return Session{}, errors.New(prompts.HistoryContinuation)
	}
	if retainFrom <= 0 || compactedAt.IsZero() || len(inputIDs) > 4 {
		return Session{}, errors.New(prompts.HistoryContinuationBoundary)
	}
	// The summary cut is frozen before the model request; later actor commits
	// may extend the retained suffix but must not move historical ancestry.
	cutFound := false
	for _, entry := range entries {
		cutFound = cutFound || entry.ID == sourceTip
	}
	if !cutFound {
		return Session{}, errors.New(prompts.HistoryCompactionSourceTip)
	}
	var pendingEntry *Entry
	if pending != nil {
		if pending.Type != llm.TypeRuntimeInstruction || pending.RequestID <= 0 {
			return Session{}, errors.New(prompts.HistoryContinuationWarning)
		}
		for i := range entries {
			entry := &entries[i]
			if !entry.Visible || entry.Actor != "main" || entry.Kind != "message" || entry.Role != pending.Role() {
				continue
			}
			var message llm.Message
			if e := json.Unmarshal(entry.Content, &message); e != nil {
				return Session{}, e
			}
			if _, added := ctxmgr.AppendPendingMessage([]llm.Message{message}, pending); !added {
				pendingEntry = entry
				break
			}
		}
		if pendingEntry == nil {
			return Session{}, errors.New(prompts.HistoryContinuationWarningMissing)
		}
	}
	selected := make(map[int64]bool, len(inputIDs))
	for i, input := range inputIDs {
		if input <= 0 || input >= retainFrom || (i > 0 && input <= inputIDs[i-1]) {
			return Session{}, errors.New(prompts.HistoryContinuationInputIDs)
		}
		selected[input] = true
	}
	id := NewID("session")
	e = s.transact(func(tx *sql.Tx) error {
		metadata, e := inputMetadataWith(tx, entries)
		if e != nil {
			return e
		}
		audit, e := mainCompactionMessagesWith(tx, session, retainFrom, entries[len(entries)-1].ID)
		if e != nil {
			return e
		}
		inputs := make(map[int64]llm.Message, len(inputIDs))
		boundaryFound := retainFrom == entries[len(entries)-1].ID+1
		normal, steers := 0, 0
		for _, entry := range entries {
			boundaryFound = boundaryFound || entry.ID == retainFrom
			if !selected[entry.ID] {
				continue
			}
			var message llm.Message
			if e := json.Unmarshal(entry.Content, &message); e != nil {
				return e
			}
			if !entry.Visible || entry.Kind != "message" || entry.Role != "user" || entry.Actor != "main" || message.Type != llm.TypeUserInput {
				return errors.New(prompts.HistoryContinuationInput)
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
			retained, e := ctxmgr.RetainedInputMessages([]llm.Message{message}, []int{0}, compactedAt)
			if e != nil {
				return e
			}
			inputs[entry.ID] = retained[0]
		}
		if !boundaryFound || len(inputs) != len(inputIDs) || normal > 2 || steers > 2 {
			return errors.New(prompts.HistoryContinuationSelection)
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
		_, e = tx.Exec(`INSERT INTO sessions(id,workspace_id,lineage_id,predecessor_id,name,name_source,model_json,last_activity_ms,metadata_json) SELECT ?,workspace_id,lineage_id,id,?,'continuation',?,last_activity_ms,metadata_json FROM sessions WHERE id=?`, id, fmt.Sprintf("%s-cont-%d", rootName, ordinal), string(m), session)
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
		summaryID, e := appendTx(tx, id, "", "main", "summary", handoff.Role(), true, data, 0)
		if e != nil {
			return e
		}
		mapping := map[int64]int64{}
		pendingCopied := false
		for _, entry := range entries {
			var marker struct{ Type string }
			stream := entry.Kind == "status" && json.Unmarshal(entry.Content, &marker) == nil && marker.Type == "presentation_stream"
			// Presentation audit survives summarization independently of model context.
			if !stream && ((entry.ID < retainFrom && !selected[entry.ID]) || audit[entry.ID]) {
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
			if entry.Visible && (entry.Kind == "message" || entry.Kind == "summary") {
				var m llm.Message
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
				if pending != nil && entry.Actor == "main" && entry.Role == pending.Role() {
					_, added := ctxmgr.AppendPendingMessage([]llm.Message{m}, pending)
					pendingCopied = pendingCopied || !added
				}
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
		if pending != nil && !pendingCopied {
			data, e := json.Marshal(pending)
			if e != nil {
				return e
			}
			source := pendingEntry.Source
			if source == 0 {
				source = pendingEntry.ID
			}
			if _, e = appendTx(tx, id, pendingEntry.TurnID, "main", "message", pending.Role(), true, data, source); e != nil {
				return e
			}
		}
		floor := summaryID
		// The exact old floor can be summarized while earlier human inputs are
		// copied separately. Keep every imported checkpoint behind the boundary.
		for _, entry := range entries {
			if entry.ID > old.UndoFloor {
				break
			}
			if mapped, ok := mapping[entry.ID]; ok {
				floor = mapped
			}
		}
		if _, e = tx.Exec("UPDATE sessions SET file_tip_id=?,undo_floor_id=? WHERE id=?", n(old.FileTip), floor, id); e != nil {
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
			if _, e = tx.Exec("UPDATE turns SET session_id=?,start_entry_id=?,start_file_tip_id=? WHERE id=? AND session_id=?", id, checkpoint, n(tip), turn, session); e != nil {
				return e
			}
		}
		// In-flight actors can have their entire visible tail summarized. Their
		// active turn still routes to the writable continuation before finishing.
		if _, e = tx.Exec("UPDATE turns SET session_id=? WHERE session_id=? AND status='running'", id, session); e != nil {
			return e
		}
		_, e = tx.Exec("INSERT INTO compactions(continuation_id,predecessor_id,source_tip_id,ordinal,summary_entry_id) VALUES(?,?,?,?,?)", id, session, sourceTip, ordinal, summaryID)
		return e
	})
	if e != nil {
		return Session{}, e
	}
	return s.Session(id)
}

// mainCompactionMessagesWith bounds audit acquisition by the indexed session
// tail. RequestMessages records the owning actor and purpose on each entry,
// including copies inherited from an older continuation.
func mainCompactionMessagesWith(tx *sql.Tx, session string, from, tip int64) (map[int64]bool, error) {
	rows, err := tx.Query(`SELECT id FROM entries
		WHERE session_id=? AND id BETWEEN ? AND ? AND kind='status'
		AND model_visible=0 AND actor_id='main'
		AND json_extract(content_json,'$.type')='request_message'
		AND json_extract(content_json,'$.purpose')='compaction'`, session, from, tip)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	audit := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		audit[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return audit, rows.Close()
}
