package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

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
	var notice struct {
		Type        string `json:"type"`
		ChildID     string `json:"child_id"`
		TurnID      string `json:"child_turn_id"`
		JobID       string `json:"job_id"`
		ResultEntry int64  `json:"result_entry_id"`
		Omitted     bool   `json:"output_omitted"`
	}
	if err := json.Unmarshal([]byte(message.Content), &notice); err != nil {
		return nil, err
	}
	if notice.Type != "child_turn_finished" || !notice.Omitted {
		return nil, nil
	}
	invalid := func() error { return fmt.Errorf(prompts.HistoryInvalidChildNotificationSource, message.EventSeq) }
	if message.EventSeq <= 0 || message.EventSeq >= entry.EventSeq() || notice.ChildID == "" ||
		!strings.HasPrefix(notice.ChildID, entry.Actor+"/child_") || strings.Contains(strings.TrimPrefix(notice.ChildID, entry.Actor+"/"), "/") {
		return nil, invalid()
	}
	var sourceActor, kind, child, turn, job, answer string
	var wrapped, sameLineage bool
	var resultEntry int64
	// Never transfer a whole source record or unbounded answer for replay. The
	// extra byte detects oversized answers rather than silently clipping them.
	err := q.QueryRow(`WITH source AS (
		SELECT session_id,actor_id,json_extract(content_json,'$.type')='runtime_event' AS wrapped,
		CASE WHEN json_extract(content_json,'$.type')='runtime_event'
		THEN json_extract(content_json,'$.body') ELSE content_json END AS body
		FROM entries WHERE id=? AND kind='status' AND source_id IS NULL
	) SELECT actor_id,wrapped,coalesce(json_extract(body,'$.type'),''),
		coalesce(json_extract(body,'$.child_id'),''),coalesce(json_extract(body,'$.child_turn_id'),''),
		coalesce(json_extract(body,'$.job_id'),''),coalesce(json_extract(body,'$.result_entry_id'),0),
		CAST(substr(CAST(coalesce(json_extract(body,'$.answer'),'') AS BLOB),1,?) AS TEXT),
		a.lineage_id=b.lineage_id AND a.workspace_id=b.workspace_id
		FROM source JOIN sessions a ON a.id=source.session_id JOIN sessions b ON b.id=?`, message.EventSeq, MaxChildAnswerBytes+1, entry.SessionID).Scan(
		&sourceActor, &wrapped, &kind, &child, &turn, &job, &resultEntry, &answer, &sameLineage)
	if err != nil {
		return nil, fmt.Errorf("resolve child notification source %d: %w", message.EventSeq, err)
	}
	owner := child
	if wrapped {
		owner = entry.Actor
	}
	if kind != "child_turn_finished" || child != notice.ChildID || sourceActor != owner || !sameLineage ||
		len(answer) > MaxChildAnswerBytes || !utf8.ValidString(answer) || resultEntry < 0 || resultEntry >= message.EventSeq ||
		notice.TurnID != "" && turn != notice.TurnID || notice.JobID != "" && job != notice.JobID ||
		notice.ResultEntry != 0 && resultEntry != notice.ResultEntry {
		return nil, invalid()
	}
	return &resolvedChildNotice{SourceID: message.EventSeq, ResultEntry: resultEntry,
		Presentation: ChildCompletionPresentation{ChildID: child, Answer: answer}}, nil
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
