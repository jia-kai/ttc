package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ChildFinish is one immutable child-turn completion. ResultEntry references the
// complete assistant message even when the live output capture was truncated.
type ChildFinish struct {
	Type        string `json:"type"`
	ChildID     string `json:"child_id"`
	TurnID      string `json:"child_turn_id"`
	JobID       string `json:"job_id"`
	Status      string `json:"status"`
	ResultEntry int64  `json:"result_entry_id,omitempty"`
	Error       string `json:"error,omitempty"`
}

// FinishChildTurn commits the terminal turn state and its single semantic event
// together. Repeating completion is rejected rather than emitting another event.
func (s *Store) FinishChildTurn(session string, finish ChildFinish) (int64, error) {
	if finish.ChildID == "" || finish.TurnID == "" || finish.JobID == "" && finish.Status == "completed" {
		return 0, errors.New("child completion requires child and turn IDs; successful assignments require a job ID")
	}
	if finish.Status != "completed" && finish.Status != "failed" && finish.Status != "cancelled" {
		return 0, errors.New("invalid child completion status")
	}
	finish.Type = "child_turn_finished"
	b, err := json.Marshal(finish)
	if err != nil {
		return 0, err
	}
	var entry int64
	err = s.transact(func(tx *sql.Tx) error {
		status := finish.Status
		if status == "cancelled" {
			status = "interrupted"
		}
		result, err := tx.Exec("UPDATE turns SET status=?,finished_ms=? WHERE id=? AND session_id=? AND actor_id=? AND status='running'", status, time.Now().UnixMilli(), finish.TurnID, session, finish.ChildID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("child turn %s is missing or already terminal", finish.TurnID)
		}
		entry, err = appendTx(tx, session, finish.TurnID, finish.ChildID, "status", "", false, b, 0)
		return err
	})
	return entry, err
}
