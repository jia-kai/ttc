package history

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"scicode/internal/provider"
)

type inputMetadata struct {
	committedMS     int64
	source, trigger string
}

// inputMetadataWith reads original admission metadata once for a branch. A JSON
// ID array keeps the query bounded independently of SQLite's parameter limit.
// Missing originals and invalid sources are rejected when a human is projected,
// rather than treating runtime notices or unretained inputs as human admissions.
func inputMetadataWith(q historyReader, entries []Entry) (map[int64]inputMetadata, error) {
	ids := make([]int64, 0)
	seen := make(map[int64]bool)
	for _, entry := range entries {
		if entry.Visible && entry.Actor == "main" && entry.Kind == "message" && entry.Role == "user" && !seen[entry.EventSeq()] {
			ids = append(ids, entry.EventSeq())
			seen[entry.EventSeq()] = true
		}
	}
	metadata := make(map[int64]inputMetadata, len(ids))
	if len(ids) == 0 {
		return metadata, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(`SELECT e.id,e.created_ms,coalesce(json_extract(e.content_json,'$.input_source'),''),coalesce(t.trigger,'')
		FROM entries e LEFT JOIN turns t ON t.id=e.turn_id
		WHERE e.id IN (SELECT value FROM json_each(?))`, string(encoded))
	if err != nil {
		return nil, fmt.Errorf("original human input metadata: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var input inputMetadata
		if err := rows.Scan(&id, &input.committedMS, &input.source, &input.trigger); err != nil {
			return nil, err
		}
		metadata[id] = input
	}
	return metadata, rows.Err()
}

// enrichInput derives human input metadata from the original immutable entry,
// not the physical continuation copy or a caller-supplied timestamp.
func enrichInput(metadata map[int64]inputMetadata, entry Entry, message provider.Message) (provider.Message, error) {
	if !entry.Visible || entry.Actor != "main" || entry.Kind != "message" || entry.Role != "user" || message.Role != "user" || message.Runtime {
		return message, nil
	}
	input, ok := metadata[entry.EventSeq()]
	if !ok {
		return provider.Message{}, fmt.Errorf("original human input #%d: %w", entry.EventSeq(), sql.ErrNoRows)
	}
	source := input.source
	if input.trigger == "steer" {
		source = "steer"
	} else if source == "" {
		source = "normal"
	}
	if source != "normal" && source != "queue" && source != "steer" {
		return provider.Message{}, fmt.Errorf("original human input #%d has invalid source %q", entry.EventSeq(), source)
	}
	message.InputSource = source
	message.InputTimeMS = input.committedMS
	return message, nil
}
