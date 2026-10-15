package migrate

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
)

const childCompletionPredicate = `(kind='status' AND (json_extract(content_json,'$.type') IN ('child_turn_finished','child_state') OR (json_extract(content_json,'$.type')='runtime_event' AND json_extract(content_json,'$.body.type')='child_turn_finished')))`

// upgrade7 only converts canonical status payloads. Model messages, saved
// requests, tool results and archives are immutable historical evidence.
func upgrade7(tx *sql.Tx, progress *progressReporter) error {
	if progress != nil {
		var count int64
		if err := tx.QueryRow("SELECT count(*) FROM entries WHERE " + childCompletionPredicate).Scan(&count); err != nil {
			return err
		}
		progress.start(count+1, "convert_child_completions")
	}
	var last int64
	for {
		updates, err := childCompletionBatch(tx, last)
		if err != nil {
			return err
		}
		if len(updates) == 0 {
			return nil
		}
		for _, u := range updates {
			if _, err := tx.Exec("UPDATE entries SET content_json=? WHERE id=?", string(u.body), u.id); err != nil {
				return err
			}
		}
		last = updates[len(updates)-1].id
		progress.advance(int64(len(updates)))
	}
}

func childCompletionBatch(tx *sql.Tx, last int64) ([]messageTypeUpdate, error) {
	rows, err := tx.Query(`SELECT id,content_json FROM entries WHERE id>? AND `+childCompletionPredicate+` ORDER BY id LIMIT ?`, last, messageTypeBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	updates := make([]messageTypeUpdate, 0, messageTypeBatchSize)
	var size int
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		body, err := canonicalChildCompletion([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", id, err)
		}
		// As with message conversion, allow one oversized entry to make progress.
		if len(updates) > 0 && size+len(body) > messageTypeBatchBytes {
			break
		}
		updates = append(updates, messageTypeUpdate{id, body})
		size += len(body)
	}
	return updates, rows.Err()
}

func canonicalChildCompletion(raw []byte) ([]byte, error) {
	outer, err := completionObject(raw)
	if err != nil {
		return nil, err
	}
	if text(outer, "type") == "child_state" {
		return canonicalChildSnapshot(raw, outer)
	}
	body := outer
	nested := text(outer, "type") == "runtime_event"
	if nested {
		body, err = completionObject(outer["body"])
		if err != nil {
			return nil, fmt.Errorf("runtime event body: %w", err)
		}
	}
	if text(body, "type") != "child_turn_finished" {
		return nil, fmt.Errorf("expected child_turn_finished")
	}
	outcomeKey := ""
	for _, key := range []string{"status", "assignment_status", "outcome"} {
		if _, ok := body[key]; ok {
			if outcomeKey != "" {
				return nil, fmt.Errorf("dual child completion outcome spellings %q and %q", outcomeKey, key)
			}
			outcomeKey = key
		}
	}
	if outcomeKey == "" {
		return nil, fmt.Errorf("missing child completion outcome")
	}
	if err := completionEnum(body[outcomeKey], "outcome", "completed", "failed", "cancelled"); err != nil {
		return nil, err
	}
	_, childState := body["child_state"]
	_, contextState := body["context_state"]
	if childState && contextState || outcomeKey == "outcome" && childState || outcomeKey != "outcome" && contextState {
		return nil, fmt.Errorf("incompatible child completion state spellings")
	}
	state := json.RawMessage(`"unknown"`)
	if outcomeKey == "outcome" {
		if !contextState {
			return nil, fmt.Errorf("missing canonical context_state")
		}
		state = body["context_state"]
	} else if childState {
		state = body["child_state"]
	}
	if err := completionEnum(state, "context_state", "idle", "closed", "unknown"); err != nil {
		return nil, err
	}
	persistent := false
	if raw, present := body["persistent"]; present {
		// Decoding null into a bool succeeds without assigning it. Require a
		// JSON boolean explicitly, even when this state does not need retention.
		var value *bool
		if err := json.Unmarshal(raw, &value); err != nil || value == nil {
			return nil, fmt.Errorf("invalid child completion persistent: %s", raw)
		}
		persistent = *value
	}
	// Missing legacy observations stay unknown; only an observed idle state
	// requires a successfully completed, retained child, as in readChildFinish.
	var observedState string
	if err := json.Unmarshal(state, &observedState); err != nil {
		return nil, err
	}
	if observedState == "idle" && (text(body, outcomeKey) != "completed" || !persistent) {
		return nil, fmt.Errorf("idle child completion requires completed outcome and persistent=true")
	}
	if outcomeKey == "outcome" {
		return raw, nil // Exact canonical fixtures need no serialization changes.
	}
	body["outcome"] = body[outcomeKey]
	body["context_state"] = state
	delete(body, outcomeKey)
	delete(body, "child_state")
	if nested {
		outer["body"], err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(outer)
}

func canonicalChildSnapshot(raw []byte, outer map[string]json.RawMessage) ([]byte, error) {
	child, err := completionObject(outer["child"])
	if err != nil {
		return nil, fmt.Errorf("child snapshot: %w", err)
	}
	stateKey, err := snapshotSpelling(child, "state", "child_state", "context_state")
	if err != nil {
		return nil, err
	}
	outcomeKey, err := snapshotSpelling(child, "assignment_status", "outcome")
	if err != nil {
		return nil, err
	}
	state, outcome := json.RawMessage(`"unknown"`), json.RawMessage(`"unknown"`)
	if stateKey != "" {
		state = child[stateKey]
	}
	if outcomeKey != "" {
		outcome = child[outcomeKey]
	}
	if err := completionEnum(state, "context_state", "running", "idle", "closed", "unknown"); err != nil {
		return nil, err
	}
	if err := completionEnum(outcome, "outcome", "running", "completed", "failed", "cancelled", "unknown"); err != nil {
		return nil, err
	}
	if stateKey == "context_state" && outcomeKey == "outcome" {
		return raw, nil
	}
	delete(child, "state")
	delete(child, "child_state")
	delete(child, "assignment_status")
	child["context_state"], child["outcome"] = state, outcome
	outer["child"], err = json.Marshal(child)
	if err != nil {
		return nil, err
	}
	return json.Marshal(outer)
}

func snapshotSpelling(body map[string]json.RawMessage, keys ...string) (string, error) {
	found := ""
	for _, key := range keys {
		if _, ok := body[key]; ok {
			if found != "" {
				return "", fmt.Errorf("dual child snapshot spellings %q and %q", found, key)
			}
			found = key
		}
	}
	return found, nil
}

func completionEnum(raw json.RawMessage, name string, values ...string) error {
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		for _, allowed := range values {
			if value == allowed {
				return nil
			}
		}
	}
	return fmt.Errorf("invalid child completion %s: %s", name, raw)
}

// completionObject rejects duplicate keys at the migration boundary rather than
// silently choosing one spelling. Raw values preserve unknown numeric precision.
func completionObject(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("child completion is not an object")
	}
	body := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("invalid object key")
		}
		if _, exists := body[key]; exists {
			return nil, fmt.Errorf("duplicate child completion key %q", key)
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		body[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing child completion data")
	}
	return body, nil
}
