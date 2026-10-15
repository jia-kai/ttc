package migrate

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"ttc/internal/llm"
)

const messageTypeBatchSize = 64
const messageTypeBatchBytes = 8 << 20

const messageTypePredicate = `(kind IN ('message','summary') OR (kind='status' AND json_extract(content_json,'$.type')='request_message'))`

// upgrade6 changes canonical entry payloads in bounded keyset batches. Saved
// provider requests and archives are immutable evidence, not current decoder inputs.
func upgrade6(tx *sql.Tx, progress *progressReporter) error {
	if progress != nil {
		var count int64
		if err := tx.QueryRow("SELECT count(*) FROM entries WHERE " + messageTypePredicate).Scan(&count); err != nil {
			return err
		}
		// Message rows, the table rebuild, and the schema-version update.
		progress.start(count+2, "convert_messages")
	}
	var last int64
	for {
		updates, err := messageTypeBatch(tx, last)
		if err != nil {
			return err
		}
		if len(updates) == 0 {
			break
		}
		for _, u := range updates {
			if _, err := tx.Exec("UPDATE entries SET content_json=? WHERE id=?", string(u.body), u.id); err != nil {
				return err
			}
		}
		last = updates[len(updates)-1].id
		progress.advance(int64(len(updates)))
	}
	progress.phase("rebuild_entries")
	if err := permitSystemRole(tx); err != nil {
		return err
	}
	progress.advance(1)
	progress.phase("rebuild_entries")
	return nil
}

type messageTypeUpdate struct {
	id   int64
	body []byte
}

func messageTypeBatch(tx *sql.Tx, last int64) ([]messageTypeUpdate, error) {
	rows, err := tx.Query(`SELECT id,kind,coalesce(role,''),content_json FROM entries WHERE id>? AND `+messageTypePredicate+` ORDER BY id LIMIT ?`, last, messageTypeBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	updates := make([]messageTypeUpdate, 0, messageTypeBatchSize)
	var bytes int
	for rows.Next() {
		var id int64
		var kind, role, raw string
		if err := rows.Scan(&id, &kind, &role, &raw); err != nil {
			return nil, err
		}
		body, err := semanticEntry(kind, role, []byte(raw))
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", id, err)
		}
		// Bound retained converted payloads as well as row count. One oversized
		// entry must still make progress; a deferred row is reread next batch.
		if len(updates) > 0 && bytes+len(body) > messageTypeBatchBytes {
			break
		}
		updates = append(updates, messageTypeUpdate{id, body})
		bytes += len(body)
	}
	return updates, rows.Err()
}

func semanticEntry(kind, role string, raw []byte) ([]byte, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	target := body
	if kind == "status" {
		target = nil // A nested decode must not merge message keys into its status.
		if err := json.Unmarshal(body["message"], &target); err != nil {
			return nil, fmt.Errorf("request message: %w", err)
		}
		role = text(body, "role")
	}
	if err := semanticMessage(target, role); err != nil {
		return nil, err
	}
	if kind == "status" {
		encoded, err := json.Marshal(target)
		if err != nil {
			return nil, err
		}
		body["message"] = encoded
	}
	return json.Marshal(body)
}

func semanticMessage(m map[string]json.RawMessage, indexedRole string) error {
	if m == nil {
		return fmt.Errorf("message is not an object")
	}
	role := text(m, "role")
	if role == "" || (indexedRole != "" && indexedRole != role) {
		return fmt.Errorf("incompatible legacy message roles %q and %q", role, indexedRole)
	}
	var runtime bool
	if raw, ok := m["runtime"]; ok {
		if err := json.Unmarshal(raw, &runtime); err != nil {
			return fmt.Errorf("invalid legacy runtime flag: %w", err)
		}
	}
	var typ llm.MessageType
	switch role {
	case "user":
		typ = llm.TypeUserInput
	case "assistant":
		typ = llm.TypeAssistant
	case "tool":
		typ = llm.TypeToolResult
	case "system":
		typ = llm.TypeSystemInstruction
	case "developer":
		typ = llm.TypeDeveloperInstruction
	default:
		return fmt.Errorf("unknown legacy message role %q", role)
	}
	if runtime {
		var payload struct {
			Type string `json:"type"`
		}
		// Content inspection is confined to legacy decoding; live constructors assign
		// types from trusted runtime control flow, never from user content.
		_ = json.Unmarshal([]byte(text(m, "content")), &payload)
		switch role {
		case "user":
			typ = llm.TypeRuntimeNotification
			if payload.Type == "child_message" {
				typ = llm.TypeChildGuidance
			}
		case "developer":
			typ = llm.TypeRuntimeInstruction
			if payload.Type == "runtime_context" {
				typ = llm.TypeRuntimeContext
			}
		default:
			return fmt.Errorf("legacy runtime flag is incompatible with role %q", role)
		}
	}
	delete(m, "role")
	delete(m, "runtime")
	m["type"], _ = json.Marshal(typ)
	// Apply current invariants after decoding historical classification. Unknown
	// fields/native payloads remain untouched in the map, not remarshal copies.
	encoded, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var canonical llm.Message
	return json.Unmarshal(encoded, &canonical)
}
