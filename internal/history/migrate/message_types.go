package migrate

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"ttc/internal/llm"
)

const messageTypeBatchSize = 64

// upgrade6 changes canonical entry payloads in bounded keyset batches. Saved
// provider requests and archives are immutable evidence, not current decoder inputs.
func upgrade6(tx *sql.Tx) error {
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
	}
	return permitSystemRole(tx)
}

type messageTypeUpdate struct {
	id   int64
	body []byte
}

func messageTypeBatch(tx *sql.Tx, last int64) ([]messageTypeUpdate, error) {
	rows, err := tx.Query(`SELECT id,kind,coalesce(role,''),content_json FROM entries WHERE id>? AND (kind IN ('message','summary') OR (kind='status' AND json_extract(content_json,'$.type')='request_message')) ORDER BY id LIMIT ?`, last, messageTypeBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	updates := make([]messageTypeUpdate, 0, messageTypeBatchSize)
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
		updates = append(updates, messageTypeUpdate{id, body})
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
