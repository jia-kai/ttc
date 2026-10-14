// Package migrate upgrades canonical history without changing saved model requests
// or archive files. The caller owns the transaction and must roll it back on error.
package migrate

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// CurrentVersion is the schema version produced by Apply.
const CurrentVersion = 6

// legacyMarker is the fixed schema-5 storage signature, not live model guidance.
const legacyMarker = `The following user message is an original human input retained across compaction,
not a newly submitted instruction. Its text and attachments are unchanged. The
metadata identifies its source and original commit time; age_ms is its age at
compaction, in milliseconds. Sources normal and queue are ordinary prompts,
steer is an interruption, task is an assigned child task, and btw is an aside.
Interpret retained inputs in their chronological context alongside the summary
and recent model/tool work; retention does not mean earlier work is unfinished.
`

type metadata struct {
	Type      string `json:"type"`
	Source    string `json:"source"`
	Original  int64  `json:"original_committed_at_ms"`
	Compacted int64  `json:"compaction_at_ms"`
	Age       *int64 `json:"age_ms"`
}
type entry struct {
	id, parent, source, created int64
	actor, kind, role, trigger  string
	visible                     bool
	message                     map[string]json.RawMessage
}

// Apply runs ordered upgrades from schema 5 to CurrentVersion inside tx. Version
// 6 is a no-op; older and newer versions are rejected. It never commits tx.
func Apply(tx *sql.Tx) error {
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < MinimumVersion || version > CurrentVersion {
		return fmt.Errorf("unsupported history schema version %d (supported: %d–%d)", version, MinimumVersion, CurrentVersion)
	}
	if version == CurrentVersion {
		return nil
	}
	if err := upgrade5(tx); err != nil {
		return fmt.Errorf("migrate history schema 5 to 6: %w", err)
	}
	_, err := tx.Exec("PRAGMA user_version = 6")
	return err
}

func upgrade5(tx *sql.Tx) error {
	// Only human inputs and candidate developer markers need payloads. Assistant
	// responses, tool results and status records can dominate historical size.
	rows, err := tx.Query(`SELECT e.id,coalesce(e.parent_id,0),coalesce(e.source_id,0),e.created_ms,e.actor_id,e.kind,coalesce(e.role,''),e.model_visible,
		CASE WHEN e.kind='message' AND (e.role IN ('user','developer') OR json_extract(e.content_json,'$.role') IN ('user','developer')) THEN e.content_json ELSE NULL END,
		coalesce(t.trigger,'') FROM entries e LEFT JOIN turns t ON t.id=e.turn_id ORDER BY e.id`)
	if err != nil {
		return err
	}
	entries := map[int64]*entry{}
	children := map[int64][]*entry{}
	var ordered []*entry
	markers := map[int64]metadata{}
	for rows.Next() {
		e := new(entry)
		var raw sql.NullString
		if err := rows.Scan(&e.id, &e.parent, &e.source, &e.created, &e.actor, &e.kind, &e.role, &e.visible, &raw, &e.trigger); err != nil {
			rows.Close()
			return err
		}
		if raw.Valid {
			if err := json.Unmarshal([]byte(raw.String), &e.message); err != nil {
				rows.Close()
				return fmt.Errorf("entry %d: %w", e.id, err)
			}
		}
		content := text(e.message, "content")
		if (e.role == "developer" || text(e.message, "role") == "developer") && strings.HasPrefix(content, legacyMarker) {
			m, err := decodeMarker(e, content)
			if err != nil {
				rows.Close()
				return fmt.Errorf("marker %d: %w", e.id, err)
			}
			markers[e.id] = m
		}
		if e.role != "user" && text(e.message, "role") != "user" {
			e.message = nil // Developer payloads are now represented by metadata.
		}
		entries[e.id] = e
		children[e.parent] = append(children[e.parent], e)
		ordered = append(ordered, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(markers) == 0 {
		return nil
	}
	// Restoring either an original or copied pre-input marker cut can admit a
	// fresh human on that branch. The admission and its copies are fresh only
	// relative to that marker lineage; a later compaction can retain the input.
	freshAdmissions := map[int64]int64{} // Human origin ID -> canonical preceding marker ID.
	for _, e := range ordered {
		if _, ok := markers[e.parent]; ok && e.source == 0 && e.visible && e.actor == entries[e.parent].actor && e.kind == "message" && e.role == "user" && text(e.message, "role") == "user" && string(e.message["runtime"]) != "true" {
			markerOrigin, err := original(entries, entries[e.parent])
			if err != nil {
				return err
			}
			freshAdmissions[e.id] = markerOrigin.id
		}
	}
	// Validate every pair and copied cursor boundary before changing any rows.
	updates := map[int64]metadata{}
	for _, e := range ordered {
		m, ok := markers[e.id]
		if !ok {
			continue
		}
		origin, err := original(entries, e)
		if err != nil {
			return err
		}
		if om, ok := markers[origin.id]; !ok || !reflect.DeepEqual(m, om) {
			return fmt.Errorf("marker %d: invalid source marker %d", e.id, origin.id)
		}
		evidence := false
		originalInputs := map[int64]bool{}
		for _, child := range children[origin.id] {
			if !child.visible || child.actor != e.actor {
				continue
			}
			inputOrigin, err := original(entries, child)
			if err != nil {
				return err
			}
			if freshAdmissions[inputOrigin.id] == origin.id {
				continue
			}
			if err := validateInput(entries, child, m); err != nil {
				return fmt.Errorf("marker %d, original input %d: %w", e.id, child.id, err)
			}
			originalInputs[inputOrigin.id] = true
		}
		if len(originalInputs) == 0 {
			return fmt.Errorf("marker %d: missing original human input evidence", e.id)
		}
		candidates := children[e.id]
		if e.source != 0 {
			candidates = append(append([]*entry{}, candidates...), children[origin.id]...)
		}
		for _, child := range candidates {
			if !child.visible || child.actor != e.actor {
				continue
			}
			inputOrigin, err := original(entries, child)
			if err != nil {
				return err
			}
			if freshAdmissions[inputOrigin.id] == origin.id {
				continue
			}
			if err := validateInput(entries, child, m); err != nil {
				return fmt.Errorf("marker %d, input %d: %w", e.id, child.id, err)
			}
			if !originalInputs[inputOrigin.id] {
				return fmt.Errorf("marker %d, input %d: copied pair source mismatch", e.id, child.id)
			}
			evidence = true
			// Original evidence for a copied cursor cut is validated, not newly retained.
			if child.parent == e.id {
				updates[child.id] = m
			}
		}
		if !evidence {
			return fmt.Errorf("marker %d: missing original human input evidence", e.id)
		}
	}
	// Cursor columns have no permanent indexes. Stage marker identities once,
	// then scan each session/turn table once rather than once per marker.
	if _, err := tx.Exec(`CREATE TEMP TABLE retention_cuts (id INTEGER PRIMARY KEY, parent_id INTEGER)`); err != nil {
		return err
	}
	for _, e := range ordered {
		if m, ok := updates[e.id]; ok {
			e.message["input_time_ms"] = json.RawMessage(fmt.Sprint(m.Original))
			source, _ := json.Marshal(m.Source)
			e.message["input_source"] = source
			e.message["retained_at_ms"] = json.RawMessage(fmt.Sprint(m.Compacted))
			raw, err := json.Marshal(e.message)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE entries SET content_json=? WHERE id=?", string(raw), e.id); err != nil {
				return err
			}
		}
		if m, ok := markers[e.id]; ok {
			raw, err := json.Marshal(struct {
				Type         string `json:"type"`
				RetainedAtMS int64  `json:"retained_at_ms"`
			}{"retention_boundary", m.Compacted})
			if err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE entries SET kind='status',role=NULL,model_visible=0,content_json=? WHERE id=?", string(raw), e.id); err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO retention_cuts VALUES(?,NULLIF(?,0))", e.id, e.parent); err != nil {
				return err
			}
		}
	}
	for _, query := range []string{
		`UPDATE sessions SET
		active_entry_id=CASE WHEN active_entry_id IN (SELECT id FROM retention_cuts) THEN (SELECT parent_id FROM retention_cuts WHERE id=active_entry_id) ELSE active_entry_id END,
		redo_entry_id=CASE WHEN redo_entry_id IN (SELECT id FROM retention_cuts) THEN (SELECT parent_id FROM retention_cuts WHERE id=redo_entry_id) ELSE redo_entry_id END,
		undo_floor_id=CASE WHEN undo_floor_id IN (SELECT id FROM retention_cuts) THEN (SELECT parent_id FROM retention_cuts WHERE id=undo_floor_id) ELSE undo_floor_id END
		WHERE active_entry_id IN (SELECT id FROM retention_cuts) OR redo_entry_id IN (SELECT id FROM retention_cuts) OR undo_floor_id IN (SELECT id FROM retention_cuts)`,
		`UPDATE turns SET start_entry_id=(SELECT parent_id FROM retention_cuts WHERE id=start_entry_id) WHERE start_entry_id IN (SELECT id FROM retention_cuts)`,
		`DROP TABLE retention_cuts`,
	} {
		if _, err := tx.Exec(query); err != nil {
			return err
		}
	}
	return nil
}
func text(m map[string]json.RawMessage, key string) string {
	var s string
	_ = json.Unmarshal(m[key], &s)
	return s
}
func decodeMarker(e *entry, content string) (metadata, error) {
	var m metadata
	if e.role != "developer" || text(e.message, "role") != "developer" || !e.visible {
		return m, fmt.Errorf("invalid developer marker row")
	}
	if len(e.message) != 2 {
		return m, fmt.Errorf("marker must contain only role and content")
	}
	payload, ok := strings.CutPrefix(content, legacyMarker+"\n")
	if !ok {
		return m, fmt.Errorf("invalid marker separator")
	}
	if err := uniqueMetadataKeys(payload); err != nil {
		return m, err
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return m, fmt.Errorf("trailing marker metadata")
	}
	if m.Type != "retained_input" || m.Original <= 0 || m.Compacted < m.Original || m.Age == nil || *m.Age != m.Compacted-m.Original {
		return m, fmt.Errorf("invalid marker time or age")
	}
	switch m.Source {
	case "normal", "queue", "steer", "task", "btw":
	default:
		return m, fmt.Errorf("invalid marker source %q", m.Source)
	}
	return m, nil
}

// A regular struct decoder accepts repeated keys and case-insensitive field
// names. Historical metadata has exactly these five case-sensitive keys.
func uniqueMetadataKeys(payload string) error {
	decoder := json.NewDecoder(strings.NewReader(payload))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("marker metadata must be an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		var key string
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		// Object member tokens are strings by the JSON decoder contract.
		key = fmt.Sprint(token)
		switch key {
		case "type", "source", "original_committed_at_ms", "compaction_at_ms", "age_ms":
		default:
			return fmt.Errorf("unknown marker metadata key %q", key)
		}
		if seen[key] {
			return fmt.Errorf("duplicate marker metadata key %q", key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if len(seen) != 5 {
		return fmt.Errorf("missing marker metadata fields")
	}
	_, err = decoder.Token()
	return err
}

func original(entries map[int64]*entry, e *entry) (*entry, error) {
	seen := map[int64]bool{}
	for e.source != 0 {
		if seen[e.id] {
			return nil, fmt.Errorf("entry %d: cyclic source provenance", e.id)
		}
		seen[e.id] = true
		source, ok := entries[e.source]
		if !ok {
			return nil, fmt.Errorf("entry %d: missing source %d", e.id, e.source)
		}
		if source.actor != e.actor || source.kind != e.kind || source.role != e.role || source.visible != e.visible {
			return nil, fmt.Errorf("entry %d: incompatible source %d", e.id, e.source)
		}
		e = source
	}
	return e, nil
}
func validateInput(entries map[int64]*entry, e *entry, m metadata) error {
	if e.kind != "message" || e.role != "user" || text(e.message, "role") != "user" || string(e.message["runtime"]) == "true" {
		return fmt.Errorf("following entry is not a human input")
	}
	origin, err := original(entries, e)
	if err != nil {
		return err
	}
	if text(origin.message, "role") != "user" || string(origin.message["runtime"]) == "true" {
		return fmt.Errorf("source is not human")
	}
	source := text(origin.message, "input_source")
	if origin.actor == "main" && origin.trigger == "steer" {
		source = "steer"
	} else if source == "" {
		source = "normal"
	}
	originalTime := origin.created
	if origin.actor != "main" {
		// Child admissions carry the assigning human's time in their message;
		// unlike main inputs they were not enriched from entries.created_ms.
		if err := json.Unmarshal(origin.message["input_time_ms"], &originalTime); err != nil || originalTime <= 0 {
			return fmt.Errorf("child source lacks original input time")
		}
	}
	if originalTime != m.Original || source != m.Source {
		return fmt.Errorf("original source/time mismatch")
	}
	// Main admissions store caller-authored fields; replay enriches them from
	// created_ms and the turn trigger. Only continuation copies have canonical
	// enriched fields that can be checked against those authoritative values.
	if e.source != 0 {
		input := e
		if raw := input.message["input_time_ms"]; len(raw) > 0 && string(raw) != "0" && !bytes.Equal(raw, []byte(fmt.Sprint(m.Original))) {
			return fmt.Errorf("stored input time mismatch")
		}
		if s := text(input.message, "input_source"); s != "" && s != m.Source {
			return fmt.Errorf("stored input source mismatch")
		}
	}
	// Continuations strip provider replay state, but never human text or files.
	clean := func(message map[string]json.RawMessage) map[string]any {
		result := map[string]any{}
		for key, raw := range message {
			switch key {
			case "state", "input_time_ms", "input_source", "retained_at_ms":
				continue
			}
			var value any
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			_ = decoder.Decode(&value) // Raw values were validated on entry decoding.
			result[key] = value
		}
		return result
	}
	if !reflect.DeepEqual(clean(e.message), clean(origin.message)) {
		return fmt.Errorf("copied human content differs from original")
	}
	return nil
}
