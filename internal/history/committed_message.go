package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"ttc/internal/llm"
)

// CommittedMessage is the typed receipt for one durably recorded message.
// EntryID identifies its inspectable history evidence; CreatedMS is the committed
// Unix timestamp in milliseconds. ChildCompletion supplements omitted notices
// for human presentation only and never changes the canonical Message.
type CommittedMessage struct {
	EntryID, CreatedMS int64
	Message            llm.Message
	ChildCompletion    *ChildCompletionPresentation
}

// AppendMessage records a canonical message and returns its committed identity
// and timestamp atomically. The message type determines its role. Main messages
// enter main model input; child and aside messages remain isolated evidence.
// The receipt owns its decoded message data; failure returns no committed identity.
func (s *Store) AppendMessage(session, turn, actor string, message llm.Message) (committed CommittedMessage, err error) {
	if session == "" || actor == "" || message.Role() == "" {
		return committed, errors.New("message requires a session, actor and canonical message type")
	}
	err = s.transact(func(tx *sql.Tx) error {
		committed, err = appendMessageTx(tx, session, turn, actor, message)
		return err
	})
	if err != nil {
		return CommittedMessage{}, err
	}
	return committed, nil
}

// ReadCommittedMessage returns canonical message evidence and its original
// timestamp without exposing its storage envelope. Non-message or unknown IDs
// return ErrNotFound. Omitted child notices include source-backed presentation,
// never the child's private conversation or tool details.
func (s *Store) ReadCommittedMessage(ctx context.Context, id int64) (committed CommittedMessage, err error) {
	defer func() { err = intentError(fmt.Sprintf("read committed message #%d", id), err) }()
	q := contextHistoryReader{ctx, s.db}
	entry := Entry{ID: id, Kind: "message"}
	var data string
	if err := q.QueryRow("SELECT session_id,actor_id,coalesce(source_id,0),created_ms,content_json FROM entries WHERE id=? AND kind='message'", id).Scan(&entry.SessionID, &entry.Actor, &entry.Source, &entry.CreatedMS, &data); err != nil {
		return CommittedMessage{}, err
	}
	committed, err = decodeCommittedMessage(q, entry, json.RawMessage(data))
	if err != nil {
		return CommittedMessage{}, err
	}
	if err := ctx.Err(); err != nil {
		return CommittedMessage{}, err
	}
	return committed, nil
}

func appendMessageTx(tx *sql.Tx, session, turn, actor string, message llm.Message) (CommittedMessage, error) {
	data, err := json.Marshal(message)
	if err != nil {
		return CommittedMessage{}, err
	}
	id, err := appendTx(tx, session, turn, actor, "message", message.Role(), actor == "main", data, 0)
	if err != nil {
		return CommittedMessage{}, err
	}
	var createdMS int64
	if err := tx.QueryRow("SELECT created_ms FROM entries WHERE id=?", id).Scan(&createdMS); err != nil {
		return CommittedMessage{}, err
	}
	return decodeCommittedMessage(tx, Entry{ID: id, SessionID: session, Actor: actor, Kind: "message", CreatedMS: createdMS}, data)
}

func decodeCommittedMessage(q historyReader, entry Entry, data json.RawMessage) (CommittedMessage, error) {
	committed := CommittedMessage{EntryID: entry.ID, CreatedMS: entry.CreatedMS}
	if err := json.Unmarshal(data, &committed.Message); err != nil {
		return CommittedMessage{}, err
	}
	notice, err := resolveChildNoticeMessage(q, entry, committed.Message)
	if err != nil {
		return CommittedMessage{}, err
	}
	if notice != nil {
		committed.ChildCompletion = &notice.Presentation
	}
	return committed, nil
}
