package history

import (
	"database/sql"
	"encoding/json"
	"errors"

	ctxmgr "ttc/internal/context"
	"ttc/internal/llm"
)

// Traverse only ancestry links before acquiring candidate message bodies by PK.
// Filtering by JSON fields here would change encoding/json's case-insensitive
// field matching and error behavior; decode only the matching envelope candidates.
const recoveryInstructionQuery = `WITH RECURSIVE ancestry(id,parent_id) AS (
	SELECT e.id,e.parent_id FROM sessions s JOIN entries e ON e.id=s.active_entry_id AND e.session_id=s.id WHERE s.id=?
	UNION ALL SELECT e.id,e.parent_id FROM entries e JOIN ancestry a ON e.id=a.parent_id
) SELECT e.id,e.created_ms,e.content_json
	FROM ancestry a CROSS JOIN entries e
	WHERE e.id=a.id AND e.actor_id=? AND e.kind='message' AND e.role=? ORDER BY e.id`

func findRecoveryInstruction(q historyReader, session, actor string, message llm.Message) (CommittedMessage, bool, error) {
	rows, err := q.Query(recoveryInstructionQuery, session, actor, message.Role())
	if err != nil {
		return CommittedMessage{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var committed CommittedMessage
		var data string
		if err := rows.Scan(&committed.EntryID, &committed.CreatedMS, &data); err != nil {
			return CommittedMessage{}, false, err
		}
		if err := json.Unmarshal([]byte(data), &committed.Message); err != nil {
			return CommittedMessage{}, false, err
		}
		if _, absent := ctxmgr.AppendPendingMessage([]llm.Message{committed.Message}, &message); !absent {
			return committed, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return CommittedMessage{}, false, err
	}
	return CommittedMessage{}, false, rows.Close()
}

// EnsureRecoveryInstruction atomically ensures the actor's selected branch
// contains the exact runtime-instruction type/content/request identity. Existing
// matches return their committed message and added=false. Only main instructions
// enter main model input; child and aside instructions remain isolated evidence.
// Errors return no committed identity, including when insertion is rolled back.
func (s *Store) EnsureRecoveryInstruction(session, turn, actor string, message llm.Message) (committed CommittedMessage, added bool, err error) {
	if session == "" || actor == "" || message.Type != llm.TypeRuntimeInstruction || message.Content == "" {
		return committed, false, errors.New("recovery requires a session, actor and nonempty runtime instruction")
	}
	err = s.transact(func(tx *sql.Tx) error {
		var found bool
		var err error
		// The helper closes its candidate rows before any insertion below.
		committed, found, err = findRecoveryInstruction(tx, session, actor, message)
		if err != nil {
			return err
		}
		if found {
			return nil
		}
		committed, err = appendMessageTx(tx, session, turn, actor, message)
		added = err == nil
		return err
	})
	if err != nil {
		return CommittedMessage{}, false, err
	}
	return committed, added, nil
}
