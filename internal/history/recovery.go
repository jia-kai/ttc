package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"ttc/internal/provider"
)

// recoveryEvent is a new semantic event owned by a manual snapshot. Source
// chronology remains copied history; only this original row can be acknowledged.
type recoveryEvent struct {
	Type          string          `json:"type"`
	Body          json.RawMessage `json:"body"`
	RecoveredFrom int64           `json:"recovered_from_event_seq,omitempty"`
}

// compactionBoundaryWith examines selected chronology, not the session's newest
// request on an abandoned branch. A summary or prior recovery marks a handoff
// even when the preceding compaction request was outside the retained suffix.
func compactionBoundaryWith(q historyReader, entries []Entry) (bool, error) {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Actor != "main" {
			continue
		}
		if e.Kind == "summary" {
			return true, nil
		}
		if e.Kind != "status" {
			continue
		}
		var marker struct {
			Type    string `json:"type"`
			Request int64  `json:"request_id"`
		}
		if err := json.Unmarshal(e.Content, &marker); err != nil {
			return false, err
		}
		if marker.Type == "compaction_recovery" || marker.Type == "compaction_started" {
			return true, nil
		}
		if marker.Type != "request_admitted" {
			continue
		}
		var actor, purpose string
		if err := q.QueryRow("SELECT actor_id,purpose FROM model_requests WHERE id=?", marker.Request).Scan(&actor, &purpose); err != nil {
			return false, fmt.Errorf("recovery request: %w", err)
		}
		if actor != "main" || purpose == "naming" {
			continue
		}
		return purpose == "compaction", nil
	}
	return false, nil
}

// recoveryAncestryWith includes only the selected branch of each frozen
// predecessor. Summarized pending events still belong to a continuation's live
// runtime, but sibling branches and manual snapshot source sessions do not.
func recoveryAncestryWith(q historyReader, session string, entries []Entry) ([]Entry, error) {
	out := append([]Entry(nil), entries...)
	seen := map[string]bool{session: true}
	for {
		var predecessor string
		var tip int64
		err := q.QueryRow("SELECT predecessor_id,source_tip_id FROM compactions WHERE continuation_id=?", session).Scan(&predecessor, &tip)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		if seen[predecessor] {
			return nil, errors.New("cyclic compaction ancestry")
		}
		seen[predecessor] = true
		branch, err := branchWith(q, predecessor, tip)
		if err != nil {
			return nil, err
		}
		out = append(out, branch...)
		session = predecessor
	}
	return out, nil
}

// recoveryBody rejects non-object payloads instead of turning damaged durable
// records into empty notifications. Raw fields preserve integer precision.
func recoveryBody(body json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return nil, errors.New("recovery body must be a JSON object")
	}
	var kind string
	if err := json.Unmarshal(object["type"], &kind); err != nil || kind == "" {
		return nil, errors.New("recovery body requires a nonempty type")
	}
	if kind == "wakeup" {
		var id string
		if json.Unmarshal(object["wakeup_id"], &id) != nil || id == "" {
			return nil, errors.New("recovered wakeup requires wakeup_id")
		}
	}
	if kind == "child_turn_finished" {
		var finish ChildFinish
		if err := json.Unmarshal(body, &finish); err != nil {
			return nil, fmt.Errorf("invalid recovered child finish: %w", err)
		}
		if err := validateChildFinish(finish); err != nil {
			return nil, fmt.Errorf("invalid recovered child finish: %w", err)
		}
	}
	return object, nil
}

func recoveryCandidatesWith(q historyReader, entries, visible []Entry) ([]recoveryEvent, error) {
	carried := map[int64]bool{}
	for _, e := range visible {
		if !e.Visible || e.Kind != "tool_result" || e.Actor != "main" {
			continue
		}
		var ref struct {
			Call string `json:"call_id"`
		}
		if err := json.Unmarshal(e.Content, &ref); err != nil {
			return nil, err
		}
		var result string
		if err := q.QueryRow("SELECT result_json FROM tool_calls WHERE id=?", ref.Call).Scan(&result); err != nil {
			return nil, err
		}
		var finish struct {
			Seq int64 `json:"finish_event_seq"`
		}
		if json.Unmarshal([]byte(result), &finish) == nil && finish.Seq > 0 {
			carried[finish.Seq] = true
		}
	}
	unique := map[int64]Entry{}
	superseded := map[int64]bool{}
	for _, e := range entries {
		if e.Kind != "status" {
			continue
		}
		var event recoveryEvent
		if err := json.Unmarshal(e.Content, &event); err != nil {
			return nil, err
		}
		if event.Type != "runtime_event" && event.Type != "child_turn_finished" {
			continue
		}
		if event.RecoveredFrom < 0 || event.RecoveredFrom >= e.EventSeq() {
			return nil, errors.New("invalid recovery provenance")
		}
		if event.RecoveredFrom > 0 {
			superseded[event.RecoveredFrom] = true
		}
		unique[e.EventSeq()] = e
	}
	ids := make([]int64, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []recoveryEvent{}
	wakeup := map[string]int{}
	for _, id := range ids {
		if superseded[id] || carried[id] {
			continue
		}
		// Delivery belongs to the original semantic row, never its physical copy.
		var delivered, source sql.NullInt64
		var raw string
		if err := q.QueryRow("SELECT source_id,delivered_request_id,content_json FROM entries WHERE id=?", id).Scan(&source, &delivered, &raw); err != nil {
			return nil, err
		}
		if source.Valid {
			return nil, errors.New("recovery source is not an original event")
		}
		if delivered.Valid {
			// Coalesced older firings were never acknowledged individually. Do
			// not revive one when a newer selected firing was already admitted.
			var event recoveryEvent
			var timer struct {
				Type string `json:"type"`
				ID   string `json:"wakeup_id"`
			}
			if json.Unmarshal([]byte(raw), &event) == nil && event.Type == "runtime_event" &&
				json.Unmarshal(event.Body, &timer) == nil && timer.Type == "wakeup" && timer.ID != "" {
				if i, ok := wakeup[timer.ID]; ok {
					out[i] = recoveryEvent{}
					delete(wakeup, timer.ID)
				}
			}
			continue
		}
		var event recoveryEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return nil, err
		}
		body := event.Body
		if event.Type == "child_turn_finished" {
			body = json.RawMessage(raw)
		} else if event.Type != "runtime_event" {
			return nil, errors.New("recovery source is not a semantic event")
		}
		object, err := recoveryBody(body)
		if err != nil {
			return nil, fmt.Errorf("recover event %d: %w", id, err)
		}
		delete(object, "event_seq")
		body, err = json.Marshal(object)
		if err != nil {
			return nil, err
		}
		item := recoveryEvent{Type: "runtime_event", Body: body, RecoveredFrom: id}
		var kind, timer string
		_ = json.Unmarshal(object["type"], &kind)
		if kind == "wakeup" {
			_ = json.Unmarshal(object["wakeup_id"], &timer)
		}
		if timer != "" {
			if i, ok := wakeup[timer]; ok {
				out[i] = recoveryEvent{}
			}
			wakeup[timer] = len(out)
		}
		out = append(out, item)
	}
	filtered := out[:0]
	for _, item := range out {
		if item.RecoveredFrom != 0 {
			filtered = append(filtered, item)
		}
	}
	return filtered, nil
}

// PendingRecoveryNotifications returns undelivered notifications created by a
// boundary-recovery load. It never writes or acknowledges events. Returned user
// runtime messages own fresh event IDs, including the event_seq in their bodies.
// Ordinary sessions, read-only predecessors and fatal contexts return no notices.
func (s *Store) PendingRecoveryNotifications(ctx context.Context, session string) ([]provider.Message, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var readOnly bool
	var fatal string
	var tip int64
	if err = tx.QueryRowContext(ctx, "SELECT read_only,coalesce(json_extract(metadata_json,'$.compaction_error'),''),coalesce(active_entry_id,0) FROM sessions WHERE id=?", session).Scan(&readOnly, &fatal, &tip); err != nil {
		return nil, err
	}
	if readOnly || fatal != "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []provider.Message
	for {
		if seen[session] {
			return nil, errors.New("cyclic compaction ancestry")
		}
		seen[session] = true
		// Traverse selected parent IDs without loading model/tool payloads. Only
		// original undelivered recovery events belong to this runtime; copied
		// originals are found in its frozen predecessor, not a manual-load source.
		// CROSS JOIN fixes lookup order: selected IDs drive primary-key probes
		// rather than SQLite scanning the entire shared entries table.
		rows, err := tx.QueryContext(ctx, `WITH RECURSIVE selected(id,parent_id) AS (
			SELECT id,parent_id FROM entries WHERE id=? AND session_id=?
			UNION ALL SELECT e.id,e.parent_id FROM entries e JOIN selected a ON e.id=a.parent_id
		) SELECT e.id,e.content_json FROM selected a CROSS JOIN entries e ON e.id=a.id
		WHERE e.source_id IS NULL AND e.delivered_request_id IS NULL AND e.kind='status'
		AND json_extract(e.content_json,'$.type')='runtime_event'
		AND coalesce(json_extract(e.content_json,'$.recovered_from_event_seq'),0)<>0`, tip, session)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var raw string
			if err = rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			var event recoveryEvent
			if err = json.Unmarshal([]byte(raw), &event); err != nil {
				rows.Close()
				return nil, err
			}
			if event.RecoveredFrom <= 0 || event.RecoveredFrom >= id {
				rows.Close()
				return nil, errors.New("invalid recovery provenance")
			}
			object, err := recoveryBody(event.Body)
			if err != nil {
				rows.Close()
				return nil, err
			}
			object["event_seq"] = json.RawMessage(fmt.Sprint(id))
			body, err := json.Marshal(object)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, provider.Message{Role: "user", Runtime: true, EventSeq: id, Content: string(body)})
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err = rows.Close(); err != nil {
			return nil, err
		}
		var predecessor string
		err = tx.QueryRowContext(ctx, "SELECT predecessor_id,source_tip_id FROM compactions WHERE continuation_id=?", session).Scan(&predecessor, &tip)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		session = predecessor
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EventSeq < out[j].EventSeq })
	return out, tx.Commit()
}
