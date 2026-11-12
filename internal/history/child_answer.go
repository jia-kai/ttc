package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// ChildAnswer identifies a validated saved response and assignment-local metadata.
// Nil flags/times and empty strings mean unavailable in the saved records. The
// response body is deliberately absent: acquire it with bounded InspectPage reads.
type ChildAnswer struct {
	ResponseID                             int64
	ChildID, Label, TurnID, JobID, Outcome string
	Provider, Model, Variant               string
	Persistent, Easy                       *bool
	StartedMS, FinishedMS, ElapsedMS       *int64
}

// Canonical ChildFinish.Persistent is a bool: JSON null does not overwrite an
// earlier duplicate value. Retain that semantics while tracking unavailable data.
type savedCompletionBool struct{ value *bool }

func (b *savedCompletionBool) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		return nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	b.value = &value
	return nil
}

// childCompletionMetadata is a small canonical decoder shared by reference
// validation and answer inspection. Unknown output/native fields are not retained.
type childCompletionMetadata struct {
	Type, Kind, Label string
	ChildID           string `json:"child_id"`
	TurnID            string `json:"child_turn_id"`
	JobID             string `json:"job_id"`
	Outcome           string `json:"outcome"`
	AssignmentStatus  string `json:"assignment_status"`
	Persistent        savedCompletionBool
	Easy              *bool
	ResultEntry       int64 `json:"result_entry_id"`
	FinishEventSeq    int64 `json:"finish_event_seq"`
	Omitted           bool  `json:"output_omitted"`
	Answer            string
}

// childCompletionSourceQuery transfers only bounded presentation/reference
// fields. json_group_object preserves spelling, scalar types and duplicate order
// so Go applies its canonical case-folded/last-non-null decoding semantics. Large
// answer and label strings are bounded before transfer; validation rejects them.
const childCompletionSourceQuery = `WITH raw AS (
 SELECT session_id,actor_id,kind,coalesce(source_id,0) copied,content_json,
 coalesce((SELECT value FROM json_each(entries.content_json) f WHERE lower(f.key)='type'
  AND f.type='text' ORDER BY f.id DESC LIMIT 1),'') type
 FROM entries WHERE id=?
), source AS (
 SELECT session_id,actor_id,kind,copied,type='runtime_event' wrapped,
 CASE WHEN type='runtime_event' THEN (
  SELECT value FROM json_each(raw.content_json) f WHERE lower(f.key)='body' ORDER BY f.id DESC LIMIT 1
 ) ELSE content_json END body FROM raw
)
SELECT session_id,actor_id,kind,copied,wrapped,coalesce((
 SELECT json_group_object(key,CASE
  WHEN lower(key)='answer' AND type='text' THEN substr(value,1,8193)
  WHEN lower(key)='label' AND type='text' THEN substr(value,1,1025)
  WHEN type='true' THEN json('true') WHEN type='false' THEN json('false')
  WHEN type='null' THEN json('null') WHEN type IN ('object','array') THEN json(value) ELSE value END)
 FROM (SELECT key,type,value FROM json_each(source.body)
  WHERE lower(key) IN ('type','child_id','child_turn_id','job_id','result_entry_id',
   'outcome','persistent','label','easy','answer') ORDER BY id)
),'{}') FROM source`

func readChildCompletionSource(q historyReader, id int64) (entry Entry, metadata childCompletionMetadata, wrapped bool, err error) {
	entry.ID = id
	var raw string
	err = q.QueryRow(childCompletionSourceQuery, id).Scan(&entry.SessionID, &entry.Actor, &entry.Kind, &entry.Source, &wrapped, &raw)
	if err != nil {
		return
	}
	err = json.Unmarshal([]byte(raw), &metadata)
	// Saved completions from before the 6-KiB write limit remain readable;
	// historical source validation must not apply a new admission limit.
	if err == nil && (len(metadata.Answer) > maxSavedChildAnswerBytes || !utf8.ValidString(metadata.Answer) || len(metadata.Label) > 1024 || !utf8.ValidString(metadata.Label)) {
		err = fmt.Errorf("child completion %d exceeds bounded answer/label metadata", id)
	}
	return
}

// ReadChildAnswer uses the same immutable provenance validation as ChildResponse.
// Metadata comes from the exact saved completion (or its validated source), with
// only a turn-primary-key lookup for model/times. No current state or lineage-wide
// job/label search participates; historical missing values remain unavailable.
func (s *Store) ReadChildAnswer(ctx context.Context, id int64) (ChildAnswer, error) {
	q := contextHistoryReader{ctx, s.db}
	resolved, err := resolveChildResponseWith(q, id)
	answer := ChildAnswer{ResponseID: resolved.ResponseID}
	if err != nil || answer.ResponseID == 0 {
		return answer, err
	}
	metadata := resolved.Metadata
	answer.ChildID, answer.Label, answer.TurnID, answer.JobID = metadata.ChildID, metadata.Label, metadata.TurnID, metadata.JobID
	answer.Outcome, answer.Persistent, answer.Easy = metadata.Outcome, metadata.Persistent.value, metadata.Easy
	if answer.Outcome == "" {
		answer.Outcome = metadata.AssignmentStatus
	}
	// The foreground API's typed subagent execution record uses omitempty for
	// false. Canonical completion-only records do not imply a missing easy flag.
	if answer.Easy == nil && metadata.Kind == "subagent" {
		easy := false
		answer.Easy = &easy
	}
	if answer.TurnID == "" {
		return answer, ctx.Err()
	}
	var started, finished sql.NullInt64
	err = q.QueryRow(childAnswerTurnQuery, resolved.Entry.SessionID, answer.TurnID, answer.ChildID).Scan(&started, &finished, &answer.Provider, &answer.Model, &answer.Variant)
	if err == sql.ErrNoRows {
		return answer, ctx.Err()
	}
	if err != nil {
		return answer, fmt.Errorf("read child answer turn metadata: %w", err)
	}
	if started.Valid {
		answer.StartedMS = &started.Int64
	}
	if finished.Valid {
		answer.FinishedMS = &finished.Int64
	}
	if started.Valid && finished.Valid {
		if finished.Int64 < started.Int64 {
			return answer, fmt.Errorf("child answer assignment %s finishes before it starts", answer.TurnID)
		}
		elapsed := finished.Int64 - started.Int64
		answer.ElapsedMS = &elapsed
	}
	return answer, ctx.Err()
}

const childAnswerTurnQuery = `SELECT t.started_ms,t.finished_ms,
 coalesce(json_extract(t.model_json,'$.provider'),''),coalesce(json_extract(t.model_json,'$.model.id'),''),
 coalesce(json_extract(t.model_json,'$.variant'),'') FROM turns t
 JOIN sessions a ON a.id=t.session_id JOIN sessions b ON b.id=?
 WHERE t.id=? AND t.actor_id=? AND a.lineage_id=b.lineage_id AND a.workspace_id=b.workspace_id`
