package migrate

import (
	"database/sql"
	"encoding/json"
	"fmt"

	ctxmgr "ttc/internal/context"
	"ttc/internal/llm"
)

const observationDDL = `ALTER TABLE model_requests ADD COLUMN admission_entry_id INTEGER REFERENCES entries(id);
ALTER TABLE model_requests ADD COLUMN system_prompt_entry_id INTEGER REFERENCES entries(id);
CREATE TABLE request_observations (
 completion_entry_id INTEGER PRIMARY KEY REFERENCES entries(id),
 request_id INTEGER NOT NULL REFERENCES model_requests(id),
 admission_entry_id INTEGER REFERENCES entries(id),
 system_prompt_entry_id INTEGER REFERENCES entries(id),
 status TEXT NOT NULL CHECK (status IN ('completed','failed','interrupted')),
 usage_json TEXT CHECK (usage_json IS NULL OR json_valid(usage_json)),
 checkpoint_json TEXT CHECK (checkpoint_json IS NULL OR json_valid(checkpoint_json))
);
CREATE INDEX request_observation_request ON request_observations(request_id, completion_entry_id);`

const originalRequestEvents = `kind='status' AND source_id IS NULL
 AND json_extract(content_json,'$.type') IN ('request_admitted','system_prompt','request_finished')`

type observationRequest struct {
	session, actor                string
	admission, prompt             sql.NullInt64
	checkpoint                    sql.NullString
	hasDiagnostics, hasCompletion bool
}

type legacyCompletion struct {
	entry, request    int64
	status            string
	usage             sql.NullString
	admission, prompt sql.NullInt64
}

// upgrade11 is the only decoder of legacy request evidence. SQL projects just
// accounting fields: large inputs, prompt snapshots and attempt diagnostics never
// enter Go. Original events are traversed once, including all actors and purposes;
// copied branches keep referring to their original completion through source_id.
func upgrade11(tx *sql.Tx, progress *progressReporter) error {
	if progress != nil {
		var requests, events int64
		if err := tx.QueryRow(`SELECT count(*) FROM model_requests`).Scan(&requests); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT count(*) FROM entries WHERE ` + originalRequestEvents).Scan(&events); err != nil {
			return err
		}
		progress.start(2*requests+2*events+3, "read_request_evidence")
	}
	if _, err := tx.Exec(observationDDL); err != nil {
		return err
	}
	progress.advance(1)
	requests := map[int64]*observationRequest{}
	rows, err := tx.Query(`SELECT id,session_id,actor_id,input_json -> '$.input_estimate',
 CASE WHEN attempts_json IS NULL OR json_type(attempts_json)='null'
 OR (json_type(attempts_json)='array' AND json_array_length(attempts_json)=0) THEN 0 ELSE 1 END
 FROM model_requests ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		r := new(observationRequest)
		if err := rows.Scan(&id, &r.session, &r.actor, &r.checkpoint, &r.hasDiagnostics); err != nil {
			rows.Close()
			return err
		}
		if r.checkpoint.Valid {
			var checkpoint *ctxmgr.Checkpoint
			if err := json.Unmarshal([]byte(r.checkpoint.String), &checkpoint); err != nil {
				rows.Close()
				return fmt.Errorf("request %d checkpoint: %w", id, err)
			}
			if checkpoint != nil {
				if err := checkpoint.Validate(); err != nil {
					rows.Close()
					return fmt.Errorf("request %d checkpoint: %w", id, err)
				}
			} else {
				r.checkpoint = sql.NullString{}
			}
		}
		requests[id] = r
		progress.advance(1)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	// Select the final successful attempt, not mutable model_requests.attempts_json.
	// Missing attempts are permitted (e.g. interruption before a provider call), but
	// a present non-array is malformed legacy evidence rather than an empty list.
	rows, err = tx.Query(`SELECT id,session_id,actor_id,
 json_extract(content_json,'$.type'),json_type(content_json,'$.request_id'),json_extract(content_json,'$.request_id'),
 json_extract(content_json,'$.status'),json_type(content_json,'$.attempts'),
 (SELECT CASE WHEN json_type(a.value,'$.usage')='object' THEN json_object(
 'input_tokens',json(a.value -> '$.usage.input_tokens'),
 'output_tokens',json(a.value -> '$.usage.output_tokens'),
 'cached_input_tokens',json(a.value -> '$.usage.cached_input_tokens'),
 'cache_write_tokens',json(a.value -> '$.usage.cache_write_tokens'),
 'reasoning_output_tokens',json(a.value -> '$.usage.reasoning_output_tokens'))
 ELSE a.value -> '$.usage' END FROM json_each(
 CASE WHEN json_type(content_json,'$.attempts')='array' THEN json_extract(content_json,'$.attempts') ELSE '[]' END) a
 WHERE json_extract(a.value,'$.status')='completed' ORDER BY cast(a.key AS INTEGER) DESC LIMIT 1)
 FROM entries WHERE ` + originalRequestEvents + ` ORDER BY id`)
	if err != nil {
		return err
	}
	var completions []legacyCompletion
	for rows.Next() {
		var id int64
		var session, actor, kind string
		var requestType, status, attempts sql.NullString
		var requestID sql.NullInt64
		var usage sql.NullString
		if err := rows.Scan(&id, &session, &actor, &kind, &requestType, &requestID, &status, &attempts, &usage); err != nil {
			rows.Close()
			return fmt.Errorf("request event: %w", err)
		}
		// Request zero is a UI-only system snapshot, not request evidence.
		if kind == "system_prompt" && requestType.String == "integer" && requestID.Int64 == 0 {
			progress.advance(1)
			continue
		}
		r := requests[requestID.Int64]
		if requestType.String != "integer" || r == nil || actor != r.actor {
			rows.Close()
			return fmt.Errorf("entry %d: invalid request identity or actor", id)
		}
		switch kind {
		case "request_admitted":
			if session != r.session {
				rows.Close()
				return fmt.Errorf("entry %d: admission session does not own request %d", id, requestID.Int64)
			}
			if r.admission.Valid {
				rows.Close()
				return fmt.Errorf("request %d: duplicate original admission", requestID.Int64)
			}
			r.admission = sql.NullInt64{Int64: id, Valid: true}
		case "system_prompt":
			r.prompt = sql.NullInt64{Int64: id, Valid: true}
		case "request_finished":
			r.hasCompletion = true
			if status.String != "completed" && status.String != "failed" && status.String != "interrupted" {
				rows.Close()
				return fmt.Errorf("entry %d: invalid completion status %q", id, status.String)
			}
			if attempts.Valid && attempts.String != "null" && attempts.String != "array" {
				rows.Close()
				return fmt.Errorf("entry %d: invalid attempts type %q", id, attempts.String)
			}
			normalized, err := legacyObservationUsage(usage)
			if err != nil {
				rows.Close()
				return fmt.Errorf("entry %d: %w", id, err)
			}
			if status.String != "completed" {
				normalized = sql.NullString{}
			}
			// Bind ownership at this event, never to a later prompt or admission
			// that is absent from an earlier selected history cut.
			completions = append(completions, legacyCompletion{entry: id, request: requestID.Int64, status: status.String, usage: normalized, admission: r.admission, prompt: r.prompt})
		}
		progress.advance(1)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	progress.phase("write_request_observations")
	for id, r := range requests {
		if r.hasDiagnostics && !r.hasCompletion {
			return fmt.Errorf("request %d: mutable attempt diagnostics have no original request_finished event", id)
		}
		if _, err := tx.Exec(`UPDATE model_requests SET admission_entry_id=?,system_prompt_entry_id=? WHERE id=?`, r.admission, r.prompt, id); err != nil {
			return err
		}
		progress.advance(1)
	}
	for _, c := range completions {
		r := requests[c.request]
		if _, err := tx.Exec(`INSERT INTO request_observations(completion_entry_id,request_id,admission_entry_id,system_prompt_entry_id,status,usage_json,checkpoint_json) VALUES(?,?,?,?,?,?,?)`, c.entry, c.request, c.admission, c.prompt, c.status, c.usage, r.checkpoint); err != nil {
			return err
		}
		progress.advance(1)
	}
	progress.phase("remove_request_attempts")
	if _, err := tx.Exec(`ALTER TABLE model_requests DROP COLUMN attempts_json`); err != nil {
		return err
	}
	progress.advance(1)
	return nil
}

func legacyObservationUsage(raw sql.NullString) (sql.NullString, error) {
	if !raw.Valid {
		return sql.NullString{}, nil
	}
	var usage *struct {
		llm.Usage
		Input *int `json:"input_tokens"`
	}
	if err := json.Unmarshal([]byte(raw.String), &usage); err != nil {
		return sql.NullString{}, fmt.Errorf("invalid usage: %w", err)
	}
	if usage == nil || usage.Input == nil {
		return sql.NullString{}, nil
	}
	usage.InputTokens = *usage.Input
	if !usage.Usage.Usable() {
		// Legacy providers sometimes reported negative sentinel counts. Keep
		// their immutable audit bytes, but do not expose them as measurements.
		return sql.NullString{}, nil
	}
	encoded, err := json.Marshal(usage.Usage)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(encoded), Valid: true}, nil
}
