package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"ttc/internal/llm"
)

// ErrNotFound means a required durable record does not exist.
var ErrNotFound = errors.New("history record not found")

func intentError(operation string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}

// WorkspacePath returns the absolute workspace path, or ErrNotFound for an
// unknown workspace. It does not inspect or restore the filesystem.
func (s *Store) WorkspacePath(ctx context.Context, workspace string) (string, error) {
	var path string
	err := s.db.QueryRowContext(ctx, "SELECT path FROM workspaces WHERE id=?", workspace).Scan(&path)
	return path, intentError("read workspace path", err)
}

// SessionTip freezes the selected conversation tip in one read. Zero means an
// empty session; an unknown session returns ErrNotFound.
func (s *Store) SessionTip(ctx context.Context, session string) (int64, error) {
	var tip int64
	err := s.db.QueryRowContext(ctx, "SELECT coalesce(active_entry_id,0) FROM sessions WHERE id=?", session).Scan(&tip)
	return tip, intentError("read session tip", err)
}

// ClaimSessionNaming atomically claims the sole automatic naming attempt and
// records its turn. False means absent, already claimed, or no longer default-
// named; it is not an error. Cancellation or storage errors never grant a claim.
func (s *Store) ClaimSessionNaming(ctx context.Context, session, turn string) (bool, error) {
	result, err := s.db.ExecContext(ctx, "UPDATE sessions SET naming_claimed=1,metadata_json=json_set(metadata_json,'$.naming_turn_id',?) WHERE id=? AND naming_claimed=0 AND name_source='default'", turn, session)
	if err != nil {
		return false, intentError("claim session naming", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, intentError("claim session naming", err)
	}
	return n > 0, nil
}

// SetAutomaticSessionName applies a generated title only while the name remains
// default. False means absent or renamed (including a concurrent manual rename).
// Title policy belongs to the caller; this operation never overwrites that rename.
func (s *Store) SetAutomaticSessionName(ctx context.Context, session, name string) (bool, error) {
	result, err := s.db.ExecContext(ctx, "UPDATE sessions SET name=?,name_source='auto' WHERE id=? AND name_source='default'", name, session)
	if err != nil {
		return false, intentError("set automatic session name", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, intentError("set automatic session name", err)
	}
	return n > 0, nil
}

// ToolExecution returns the request metadata needed to execute an admitted call.
// Missing intent/request records return ErrNotFound; malformed saved models fail.
func (s *Store) ToolExecution(ctx context.Context, call string) (ToolExecution, error) {
	var value ToolExecution
	var model string
	err := s.db.QueryRowContext(ctx, "SELECT e.id,c.request_id,m.model_json FROM entries e JOIN tool_calls c ON c.id=json_extract(e.content_json,'$.call_id') JOIN model_requests m ON m.id=c.request_id WHERE e.kind='tool_call' AND c.id=? ORDER BY e.id DESC LIMIT 1", call).Scan(&value.EntryID, &value.RequestID, &model)
	if err == nil {
		err = json.Unmarshal([]byte(model), &value.Selection)
	}
	if err != nil {
		return ToolExecution{}, intentError("read tool execution", err)
	}
	return value, nil
}

// ToolSelection returns the model selection frozen at a call's owning request,
// without scanning conversation entries. An unknown call/request returns
// ErrNotFound; malformed saved model data returns a decoding error.
func (s *Store) ToolSelection(ctx context.Context, call string) (llm.Selection, error) {
	var model string
	err := s.db.QueryRowContext(ctx, "SELECT q.model_json FROM tool_calls c JOIN model_requests q ON q.id=c.request_id WHERE c.id=?", call).Scan(&model)
	var selection llm.Selection
	if err == nil {
		err = json.Unmarshal([]byte(model), &selection)
	}
	if err != nil {
		return llm.Selection{}, intentError("read tool selection", err)
	}
	return selection, nil
}

// ToolIntentEntry returns the latest physical intent entry for a call, including
// archived sessions. It returns ErrNotFound when no intent was recorded.
func (s *Store) ToolIntentEntry(ctx context.Context, call string) (int64, error) {
	var entry int64
	err := s.db.QueryRowContext(ctx, "SELECT id FROM entries WHERE kind='tool_call' AND json_extract(content_json,'$.call_id')=? ORDER BY id DESC LIMIT 1", call).Scan(&entry)
	return entry, intentError("read tool intent entry", err)
}

// ToolInspectionEntry finds a saved result entry, falling back to the latest
// intent while a call is pending. Zero and false mean neither exists.
func (s *Store) ToolInspectionEntry(ctx context.Context, call string) (int64, bool, error) {
	var entry sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT coalesce((SELECT entry_id FROM tool_records WHERE call_id=?),(SELECT id FROM entries WHERE kind='tool_call' AND json_extract(content_json,'$.call_id')=? ORDER BY id DESC LIMIT 1))", call, call).Scan(&entry)
	if err != nil {
		return 0, false, intentError("read tool inspection entry", err)
	}
	return entry.Int64, entry.Valid, nil
}

// ToolOutcomeForEntry resolves either an intent or result entry to its call and
// original execution result, not the potentially reduced model delivery.
// Unrelated or missing entries return ErrNotFound. Result bytes are caller-owned.
func (s *Store) ToolOutcomeForEntry(ctx context.Context, entry int64) (ToolOutcome, error) {
	var value ToolOutcome
	var result sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT c.id,c.name,r.record_json FROM tool_calls c LEFT JOIN tool_records r ON r.call_id=c.id WHERE c.id=coalesce((SELECT call_id FROM tool_records WHERE entry_id=?),(SELECT json_extract(content_json,'$.call_id') FROM entries WHERE id=? AND kind='tool_call')) ORDER BY r.entry_id DESC LIMIT 1", entry, entry).Scan(&value.CallID, &value.Name, &result)
	if err != nil {
		return ToolOutcome{}, intentError("read tool outcome", err)
	}
	if result.Valid {
		var record struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(result.String), &record); err != nil {
			return ToolOutcome{}, intentError("decode tool execution result", err)
		}
		if record.Result == nil {
			return ToolOutcome{}, errors.New("saved tool execution record has no result")
		}
		value.Result = record.Result
	}
	return value, nil
}
