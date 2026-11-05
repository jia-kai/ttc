package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// forkCanonicalJSON validates the canonical envelope without transferring its
// potentially huge text, provider replay items, or raw call arguments to Go.
// json_each preserves duplicate/case-folded keys for the canonical Go decoder.
// Preserve types (including malformed ones), semantic metadata and references;
// only unconstrained text and opaque JSON payloads are replaced. Unknown fields
// are ignored just as Message.UnmarshalJSON ignores them, except legacy flags.
// encoding/json folds names with unicode.SimpleFold. For ASCII schema names,
// its only non-ASCII aliases are long-s and Kelvin sign; normalize those before
// SQLite's ASCII-only lower(). Keep original keys in the output so Go remains
// authoritative about duplicate/null fields, types and semantic compatibility.
// TestForkJSONNameFoldAliases checks this against Go's complete fold cycles.
const forkCanonicalJSON = `(SELECT json_group_object(field.key,json(CASE
 WHEN lower(replace(replace(field.key,'ſ','s'),'K','k')) IN ('content','user_text') AND field.type='text' THEN '""'
 WHEN lower(replace(replace(field.key,'ſ','s'),'K','k'))='state' AND field.type='object' THEN (
  SELECT json_group_object(state.key,json(CASE WHEN lower(replace(replace(state.key,'ſ','s'),'K','k'))='items' AND state.type='array' THEN '[]'
   WHEN state.type='text' THEN json_quote(state.value) WHEN state.type IN ('true','false','null') THEN state.type ELSE state.value END))
  FROM json_each(field.value) state WHERE lower(replace(replace(state.key,'ſ','s'),'K','k')) IN ('provider','model','version','items'))
 WHEN lower(replace(replace(field.key,'ſ','s'),'K','k'))='calls' AND field.type='array' THEN (
  SELECT json_group_array(json(CASE WHEN item.type='object' THEN (
   SELECT json_group_object(call.key,json(CASE WHEN lower(replace(replace(call.key,'ſ','s'),'K','k'))='arguments' THEN '{}'
    WHEN call.type='text' THEN json_quote(call.value) WHEN call.type IN ('true','false','null') THEN call.type ELSE call.value END))
   FROM json_each(item.value) call WHERE lower(replace(replace(call.key,'ſ','s'),'K','k')) IN ('id','name','arguments'))
   WHEN item.type='text' THEN json_quote(item.value) WHEN item.type IN ('true','false','null') THEN item.type ELSE item.value END))
  FROM json_each(field.value) item)
 WHEN field.type='text' THEN json_quote(field.value)
 WHEN field.type IN ('true','false','null') THEN field.type ELSE field.value END))
 FROM json_each(e.content_json) field WHERE lower(replace(replace(field.key,'ſ','s'),'K','k')) IN
 ('event_seq','request_id','type','input_source','input_time_ms','retained_at_ms','phase','content','user_text','calls','call_id','files','state')
 OR field.key IN ('role','runtime'))`

// forkScanSQL walks only ancestry IDs before acquiring bounded semantic
// envelopes. Status bodies are loaded later only for actual recovery candidates.
const forkScanSQL = `WITH RECURSIVE ancestry(id,parent_id) AS (
 SELECT id,parent_id FROM entries WHERE id=?
 UNION ALL SELECT e.id,e.parent_id FROM ancestry a CROSS JOIN entries e WHERE e.id=a.parent_id AND e.id<a.id
) SELECT e.id,coalesce(e.parent_id,0),coalesce(e.source_id,0),coalesce(e.file_tip_id,0),e.session_id,
 coalesce(e.turn_id,''),e.actor_id,e.kind,coalesce(e.role,''),e.model_visible,e.created_ms,
 CASE WHEN e.kind IN ('message','summary') THEN ` + forkCanonicalJSON + `
 WHEN e.kind='tool_result' THEN json_object('call_id',json_extract(e.content_json,'$.call_id'))
 ELSE '{}' END
 FROM ancestry a CROSS JOIN entries e WHERE e.id=a.id ORDER BY e.id`

// forkScanWith supplies lightweight entries for balanced-cut and recovery
// selection. Content is deliberately a validation/reference projection, not an
// exportable canonical record. No source payload or ownership is modified.
func forkScanWith(q historyReader, session string, tip int64) ([]Entry, error) {
	if tip == 0 {
		if err := q.QueryRow("SELECT coalesce(active_entry_id,0) FROM sessions WHERE id=?", session).Scan(&tip); err != nil {
			return nil, err
		}
	} else if err := validateHistoryTip(q, session, tip); err != nil {
		return nil, err
	}
	rows, err := q.Query(forkScanSQL, tip)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	var parent int64
	for rows.Next() {
		var entry Entry
		var content string
		if err := rows.Scan(&entry.ID, &entry.Parent, &entry.Source, &entry.FileTip, &entry.SessionID, &entry.TurnID, &entry.Actor, &entry.Kind, &entry.Role, &entry.Visible, &entry.CreatedMS, &content); err != nil {
			return nil, err
		}
		if entry.Parent != parent || entry.Parent >= entry.ID {
			return nil, fmt.Errorf("invalid history parent #%d for #%d", entry.Parent, entry.ID)
		}
		entry.Content = json.RawMessage(content)
		if entry.Kind == "message" || entry.Kind == "summary" {
			var message llm.Message
			if err := json.Unmarshal(entry.Content, &message); err != nil {
				return nil, fmt.Errorf(prompts.HistoryDecodeCanonicalMessage, entry.Kind, err)
			}
			if message.Role() != entry.Role {
				return nil, fmt.Errorf(prompts.HistoryMessageIndexedRole, message.Type, message.Role(), entry.Role)
			}
		}
		if err := projectForkEntry(session, &entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		parent = entry.ID
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if tip != 0 && parent != tip {
		return nil, fmt.Errorf("selected history entry #%d is missing", tip)
	}
	return entries, nil
}

// forkBoundaryWith stops at the nearest selected main request or handoff. Normal
// loads never inspect older status/audit JSON, even when those bodies are huge.
func forkBoundaryWith(q historyReader, entries []Entry) (bool, error) {
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.Actor != "main" {
			continue
		}
		if entry.Kind == "summary" {
			return true, nil
		}
		if entry.Kind != "status" {
			continue
		}
		var raw string
		if err := q.QueryRow(`SELECT json_object('type',json_extract(content_json,'$.type'),
		 'request_id',json_extract(content_json,'$.request_id')) FROM entries WHERE id=?`, entry.ID).Scan(&raw); err != nil {
			return false, err
		}
		var marker struct {
			Type    string `json:"type"`
			Request int64  `json:"request_id"`
		}
		if err := json.Unmarshal([]byte(raw), &marker); err != nil {
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
		if actor == "main" && purpose != "naming" {
			return purpose == "compaction", nil
		}
	}
	return false, nil
}

// forkRecoveryEntriesWith acquires only small semantic-event references. Actual
// candidate bodies are read by recoveryCandidatesWith from original identities.
func forkRecoveryEntriesWith(q historyReader, entries []Entry) error {
	ids := make([]int64, 0)
	indices := make(map[int64]int)
	for i, entry := range entries {
		if entry.Kind == "status" {
			ids = append(ids, entry.ID)
			indices[entry.ID] = i
		}
	}
	if len(ids) == 0 {
		return nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	rows, err := q.Query(`SELECT e.id,json_object('type',json_extract(e.content_json,'$.type'),
	 'recovered_from_event_seq',json_extract(e.content_json,'$.recovered_from_event_seq'))
	 FROM json_each(?) selected CROSS JOIN entries e ON e.id=selected.value
	 WHERE json_extract(e.content_json,'$.type') IN ('runtime_event','child_turn_finished')`, string(encoded))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var content string
		if err := rows.Scan(&id, &content); err != nil {
			return err
		}
		entries[indices[id]].Content = json.RawMessage(content)
	}
	return rows.Err()
}

// forkRecoveryAncestryWith follows frozen compaction cuts with lightweight
// envelopes. The candidate selector separately checks live delivery ownership;
// readable manual-source evidence never grants acknowledgement authority.
func forkRecoveryAncestryWith(q historyReader, session string, entries []Entry) ([]Entry, error) {
	out := append([]Entry(nil), entries...)
	seen := map[string]bool{session: true}
	for {
		var predecessor string
		var tip int64
		err := q.QueryRow("SELECT predecessor_id,source_tip_id FROM compactions WHERE continuation_id=?", session).Scan(&predecessor, &tip)
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if seen[predecessor] {
			return nil, errors.New("cyclic compaction ancestry")
		}
		seen[predecessor] = true
		branch, err := forkScanWith(q, predecessor, tip)
		if err != nil {
			return nil, err
		}
		out = append(out, branch...)
		session = predecessor
	}
}
