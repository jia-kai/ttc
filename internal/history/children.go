package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"ttc/internal/prompts"
)

// MaxChildAnswerBytes bounds the final answer delivered to the parent, in UTF-8
// bytes. Full text stays in its committed assistant entry or failure transcript.
const MaxChildAnswerBytes = 8 << 10

// ChildFinish is one immutable child-turn completion. ResultEntry references the
// full committed message, possibly from a prior assignment when no new text was
// produced. It is zero when no assistant entry was committed; failure transcripts
// explicitly preserve uncommitted output without fabricating an entry ID.
type ChildFinish struct {
	Type                  string `json:"type"`
	ChildID               string `json:"child_id"`
	TurnID                string `json:"child_turn_id"`
	JobID                 string `json:"job_id"`
	Status                string `json:"status"`
	Persistent            bool   `json:"persistent"` // Retain the child context after a successful assignment.
	ResultEntry           int64  `json:"result_entry_id,omitempty"`
	Answer                string `json:"answer,omitempty"`           // Bounded final/last nonempty assistant text, never tool chatter.
	Truncated             bool   `json:"answer_truncated,omitempty"` // Answer is a prefix; inspect ResultEntry or the failure transcript for full text.
	Error                 string `json:"error,omitempty"`
	Warning               string `json:"warning,omitempty"`                 // Failure/side-effect guidance, separate from the bounded answer.
	TranscriptPath        string `json:"transcript_path,omitempty"`         // Durable complete conversation Markdown.
	TranscriptJSONLPath   string `json:"transcript_jsonl_path,omitempty"`   // Exact companion records.
	TranscriptExportError string `json:"transcript_export_error,omitempty"` // Explicit export failure; paths are absent.
}

// FinishChildTurn commits the terminal turn state and its single semantic event
// together. Repeating completion is rejected rather than emitting another event.
func (s *Store) FinishChildTurn(session string, finish ChildFinish) (int64, error) {
	if err := validateChildFinish(finish); err != nil {
		return 0, err
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
			return fmt.Errorf(prompts.HistoryChildTurnTerminal, finish.TurnID)
		}
		entry, err = appendTx(tx, session, finish.TurnID, finish.ChildID, "status", "", false, b, 0)
		return err
	})
	return entry, err
}

// validateChildFinish keeps durable writes and recovered notifications aligned.
func validateChildFinish(finish ChildFinish) error {
	if finish.ChildID == "" || finish.TurnID == "" || finish.JobID == "" && finish.Status == "completed" {
		return errors.New(prompts.HistoryChildCompletionIDs)
	}
	if finish.Status != "completed" && finish.Status != "failed" && finish.Status != "cancelled" {
		return errors.New(prompts.HistoryChildCompletionStatus)
	}
	if finish.ResultEntry < 0 {
		return errors.New(prompts.HistoryChildResultEntry)
	}
	if len(finish.Answer) > MaxChildAnswerBytes || !utf8.ValidString(finish.Answer) {
		return errors.New(prompts.HistoryChildAnswer)
	}
	if (finish.TranscriptPath == "") != (finish.TranscriptJSONLPath == "") || (finish.TranscriptPath != "" && finish.TranscriptExportError != "") {
		return errors.New(prompts.HistoryChildTranscript)
	}
	if finish.Status != "completed" && (finish.Answer != "" || finish.Truncated) && (finish.Warning == "" || finish.TranscriptPath == "" && finish.TranscriptExportError == "") {
		return errors.New(prompts.HistoryChildPartialAnswer)
	}
	return nil
}
