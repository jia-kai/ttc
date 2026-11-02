package history

import (
	"encoding/json"
	"errors"
	"fmt"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// RuntimeNotificationSource reads a canonical notification body and its recovery
// provenance. With a positive maxSourceBytes, oversized source records project to
// scalar reference metadata inside SQLite instead of returning the full payload.
// SQLite still reads/parses that source; the threshold bounds source transfer,
// not database processing or the size of arbitrary malformed metadata fields.
// The returned omitted flag requires explicit omission disclosure to the model.
// Zero requests the complete immutable body; no source is modified or acknowledged.
func (s *Store) RuntimeNotificationSource(id int64, maxSourceBytes int) (PendingNotification, bool, error) {
	if maxSourceBytes < 0 {
		return PendingNotification{}, false, errors.New("negative notification body limit")
	}
	var body string
	var recovered int64
	var omitted bool
	err := s.db.QueryRow(`WITH source AS (
		SELECT content_json AS raw,
		coalesce(json_extract(content_json,'$.recovered_from_event_seq'),0) AS recovered,
		json_extract(content_json,'$.type')='runtime_event' AS wrapped,
		? > 0 AND length(CAST(content_json AS BLOB)) > ? AS omitted
		FROM entries WHERE id=? AND kind='status' AND source_id IS NULL
	) SELECT recovered,omitted,CASE WHEN omitted THEN (
		SELECT json_group_object(key,CASE type WHEN 'true' THEN json('true')
			WHEN 'false' THEN json('false') ELSE value END)
		FROM json_each(raw,CASE WHEN wrapped THEN '$.body' ELSE '$' END)
		WHERE key IN ('type','job_id','kind','exit_code','signal','wakeup_id','fired_count',
			'status','child_id','child_turn_id','outcome','context_state','persistent','result_entry_id')
	) WHEN wrapped THEN json_extract(raw,'$.body') ELSE raw END FROM source`, maxSourceBytes, maxSourceBytes, id).Scan(&recovered, &omitted, &body)
	if err != nil {
		return PendingNotification{}, false, err
	}
	return PendingNotification{EventSeq: id, RecoveredFrom: recovered, Body: json.RawMessage(body)}, omitted, nil
}

// omittedChildNoticeSource closes an exported notification's explicit source
// reference, not the child's private conversation. Source identity must precede
// delivery and stay in its lineage; copies never become new semantic events.
func omittedChildNoticeSource(q historyReader, entry Entry) (*Entry, error) {
	if entry.Kind != "message" {
		return nil, nil
	}
	var message llm.Message
	if err := json.Unmarshal(entry.Content, &message); err != nil {
		return nil, err
	}
	if message.Type != llm.TypeRuntimeNotification || message.EventSeq <= 0 {
		return nil, nil
	}
	var notice struct {
		Type    string `json:"type"`
		ChildID string `json:"child_id"`
		Omitted bool   `json:"output_omitted"`
	}
	if err := json.Unmarshal([]byte(message.Content), &notice); err != nil {
		return nil, err
	}
	if notice.Type != "child_turn_finished" || !notice.Omitted {
		return nil, nil
	}
	source, err := entryWith(q, message.EventSeq)
	if err != nil {
		return nil, err
	}
	var sameLineage bool
	if err := q.QueryRow(`SELECT a.lineage_id=b.lineage_id AND a.workspace_id=b.workspace_id FROM sessions a JOIN sessions b ON b.id=? WHERE a.id=?`, entry.SessionID, source.SessionID).Scan(&sameLineage); err != nil {
		return nil, err
	}
	body, err := childNoticeSourceBody(source)
	if err != nil {
		return nil, err
	}
	var finish struct {
		Type    string `json:"type"`
		ChildID string `json:"child_id"`
	}
	if err := json.Unmarshal(body, &finish); err != nil {
		return nil, err
	}
	if source.Source != 0 || source.ID >= entry.EventSeq() || source.Kind != "status" || !sameLineage || finish.Type != "child_turn_finished" || finish.ChildID != notice.ChildID {
		return nil, fmt.Errorf(prompts.HistoryInvalidChildNotificationSource, message.EventSeq)
	}
	return &source, nil
}

func childNoticeSourceBody(source Entry) (json.RawMessage, error) {
	var event struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(source.Content, &event); err != nil {
		return nil, err
	}
	if event.Type == "runtime_event" {
		return event.Body, nil
	}
	return source.Content, nil
}
