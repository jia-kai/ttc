package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// NotificationSourceRef identifies an immutable source event and, on recovery,
// the prior event from which the fresh source was recovered.
type NotificationSourceRef struct {
	EventSeq      int64
	RecoveredFrom int64
}

// NotificationDelivery contains source data selected for model delivery, not a
// human presentation or the final admitted llm.Message. Body is owned by the
// caller. OutputOmitted requires explicit disclosure in the model projection.
type NotificationDelivery struct {
	Source        NotificationSourceRef
	Body          json.RawMessage
	OutputOmitted bool
}

// ReadNotificationDelivery selects canonical notification data for delivery.
// A positive maxSourceBytes projects oversized records to scalar reference
// metadata inside SQLite; zero selects the complete immutable body. The limit
// bounds source transfer, not SQLite processing or malformed metadata fields.
// No source is modified or acknowledged. The session owns model API projection
// and admission of the exact delivered llm.Message.
func (s *Store) ReadNotificationDelivery(id int64, maxSourceBytes int) (NotificationDelivery, error) {
	if maxSourceBytes < 0 {
		return NotificationDelivery{}, errors.New("negative notification body limit")
	}
	var body string
	delivery := NotificationDelivery{Source: NotificationSourceRef{EventSeq: id}}
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
	) WHEN wrapped THEN json_extract(raw,'$.body') ELSE raw END FROM source`, maxSourceBytes, maxSourceBytes, id).Scan(&delivery.Source.RecoveredFrom, &delivery.OutputOmitted, &body)
	if err != nil {
		return NotificationDelivery{}, err
	}
	delivery.Body = json.RawMessage(body)
	return delivery, nil
}

// ChildCompletionPresentation is bounded, source-backed human display data.
// It supplements an omitted delivery without changing its canonical message.
// It never follows ResultEntry or loads a child's private conversation.
type ChildCompletionPresentation struct {
	ChildID string
	Answer  string
}

type resolvedChildNotice struct {
	SourceID     int64
	ResultEntry  int64
	Presentation ChildCompletionPresentation
	Metadata     childCompletionMetadata
}

// resolveOmittedChildNotice is the shared authorization boundary for display,
// inspection and export. Only export subsequently loads the full explicit source.
func resolveOmittedChildNotice(q historyReader, entry Entry) (*resolvedChildNotice, error) {
	if entry.Kind != "message" {
		return nil, nil
	}
	var message llm.Message
	if err := json.Unmarshal(entry.Content, &message); err != nil {
		return nil, err
	}
	return resolveChildNoticeMessage(q, entry, message)
}

func resolveChildNoticeMessage(q historyReader, entry Entry, message llm.Message) (*resolvedChildNotice, error) {
	if message.Type != llm.TypeRuntimeNotification {
		return nil, nil
	}
	var notice childCompletionMetadata
	if err := json.Unmarshal([]byte(message.Content), &notice); err != nil {
		return nil, err
	}
	if notice.Type != "child_turn_finished" || !notice.Omitted {
		return nil, nil
	}
	return resolveChildNoticeSource(q, entry, message.EventSeq, notice)
}

// resolveChildNoticeSource validates the exact immutable semantic source for
// omitted and full notifications and foreground completion references alike.
func resolveChildNoticeSource(q historyReader, entry Entry, sourceID int64, notice childCompletionMetadata) (*resolvedChildNotice, error) {
	invalid := func() error { return fmt.Errorf(prompts.HistoryInvalidChildNotificationSource, sourceID) }
	if sourceID <= 0 || sourceID >= entry.EventSeq() || notice.ChildID == "" ||
		!strings.HasPrefix(notice.ChildID, entry.Actor+"/child_") || strings.Contains(strings.TrimPrefix(notice.ChildID, entry.Actor+"/"), "/") {
		return nil, invalid()
	}
	source, metadata, wrapped, err := readChildCompletionSource(q, sourceID)
	if err != nil {
		return nil, fmt.Errorf("resolve child notification source %d: %w", sourceID, err)
	}
	var sameLineage bool
	if err := q.QueryRow(`SELECT a.lineage_id=b.lineage_id AND a.workspace_id=b.workspace_id
	 FROM sessions a JOIN sessions b ON b.id=? WHERE a.id=?`, entry.SessionID, source.SessionID).Scan(&sameLineage); err != nil {
		return nil, err
	}
	owner := metadata.ChildID
	if wrapped {
		owner = entry.Actor
	}
	if source.Kind != "status" || source.Source != 0 || metadata.Type != "child_turn_finished" || metadata.ChildID != notice.ChildID || source.Actor != owner || !sameLineage ||
		metadata.ResultEntry < 0 || metadata.ResultEntry >= sourceID ||
		notice.TurnID != "" && metadata.TurnID != notice.TurnID || notice.JobID != "" && metadata.JobID != notice.JobID ||
		notice.ResultEntry != 0 && metadata.ResultEntry != notice.ResultEntry {
		return nil, invalid()
	}
	return &resolvedChildNotice{SourceID: sourceID, ResultEntry: metadata.ResultEntry, Metadata: metadata,
		Presentation: ChildCompletionPresentation{ChildID: metadata.ChildID, Answer: metadata.Answer}}, nil
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
