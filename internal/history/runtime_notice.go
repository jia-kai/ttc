package history

import (
	"encoding/hex"
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

// ReadNotificationSource validates an original committed notification event and
// returns its immutable identity without loading its body. Copied message rows
// and non-event entries are not notification sources and return ErrNotFound.
// Delivery projection and acknowledgment remain separate admission operations.
func (s *Store) ReadNotificationSource(id int64) (source NotificationSourceRef, err error) {
	defer func() { err = intentError(fmt.Sprintf("read notification source #%d", id), err) }()
	err = s.db.QueryRow(`SELECT id,coalesce(json_extract(content_json,'$.recovered_from_event_seq'),0)
		FROM entries WHERE id=? AND kind='status' AND source_id IS NULL`, id).Scan(&source.EventSeq, &source.RecoveredFrom)
	if err != nil {
		return NotificationSourceRef{}, err
	}
	return source, nil
}

// NotificationDelivery contains source data selected for model delivery, not a
// human presentation or the final admitted llm.Message. Body is owned by the
// caller. TruncatedFields requires explicit disclosure in the model projection.
type NotificationDelivery struct {
	Source NotificationSourceRef
	Body   json.RawMessage
	// TruncatedFields identifies fields shortened by the bounded source read.
	// It is delivery metadata, never part of the immutable source event.
	TruncatedFields []string
}

// ReadNotificationDelivery selects canonical notification data for delivery.
// A positive maxFieldBytes bounds each string prefix inside SQLite before it
// crosses into Go; zero selects the full immutable body. Oversized structured
// fields become null. The session applies the single final formatted-message
// limit. Neither source-size nor token estimates discard complete answers.
// No source is modified or acknowledged.
func (s *Store) ReadNotificationDelivery(id int64, maxFieldBytes int) (NotificationDelivery, error) {
	if maxFieldBytes < 0 {
		return NotificationDelivery{}, errors.New("negative notification body limit")
	}
	var body, clipped string
	delivery := NotificationDelivery{Source: NotificationSourceRef{EventSeq: id}}
	err := s.db.QueryRow(`WITH source AS (
		SELECT content_json AS raw,
		coalesce(json_extract(content_json,'$.recovered_from_event_seq'),0) AS recovered,
		CASE WHEN json_extract(content_json,'$.type')='runtime_event'
		 THEN json_extract(content_json,'$.body') ELSE content_json END AS body
		FROM entries WHERE id=? AND kind='status' AND source_id IS NULL
	), fields AS (
		SELECT key,type,value,? > 0 AND type IN ('text','object','array')
		 AND length(CAST(value AS BLOB)) > ? AS clipped FROM source,json_each(body)
	) SELECT recovered,CASE WHEN ?=0 THEN body ELSE (
		SELECT json_group_object(key,CASE
		 WHEN type='text' AND clipped THEN hex(substr(CAST(value AS BLOB),1,?))
		 WHEN type IN ('object','array') AND clipped THEN json('null')
		 WHEN type IN ('object','array') THEN json(value)
		 WHEN type='true' THEN json('true') WHEN type='false' THEN json('false')
		 WHEN type='null' THEN json('null') ELSE value END) FROM fields
	) END, (SELECT json_group_array(key) FROM fields WHERE clipped) FROM source`, id, maxFieldBytes, maxFieldBytes, maxFieldBytes, maxFieldBytes).Scan(&delivery.Source.RecoveredFrom, &body, &clipped)
	if err != nil {
		return NotificationDelivery{}, err
	}
	if err := json.Unmarshal([]byte(clipped), &delivery.TruncatedFields); err != nil {
		return NotificationDelivery{}, err
	}
	// SQLite text substr stops at embedded NUL. Slice bytes and hex-encode in
	// SQL instead, then repair a potentially split final UTF-8 rune in Go. This
	// transfers at most twice the prefix allowance, independent of source size.
	if len(delivery.TruncatedFields) != 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &fields); err != nil {
			return NotificationDelivery{}, err
		}
		for _, key := range delivery.TruncatedFields {
			var text string
			if len(fields[key]) != 0 && fields[key][0] == '"' && json.Unmarshal(fields[key], &text) == nil {
				prefix, err := hex.DecodeString(text)
				if err != nil {
					return NotificationDelivery{}, err
				}
				// JSON source strings are valid UTF-8; only the final rune may
				// have been split by the byte slice (at most three removals).
				for len(prefix) > 0 && !utf8.Valid(prefix) {
					prefix = prefix[:len(prefix)-1]
				}
				fields[key], _ = json.Marshal(string(prefix))
			}
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			return NotificationDelivery{}, err
		}
		body = string(encoded)
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
