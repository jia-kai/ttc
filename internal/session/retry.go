package session

import (
	"errors"
	"fmt"

	"scicode/internal/provider"
)

// retryNotice commits an inspectable system message without adding model input.
// Route locking keeps child notices in the active compaction continuation.
// Naming retries remain notices without replacing foreground UI activity.
func (r *Runtime) retryNotice(turn, actor string, request int64, purpose string, retry *provider.Retry) error {
	if retry == nil || retry.Attempt < 2 || retry.MaxAttempts < 0 || retry.MaxAttempts > 0 && retry.Attempt > retry.MaxAttempts || retry.DelayMilliseconds < 0 || retry.Reason == "" {
		return errors.New("provider emitted invalid retry notice")
	}
	attempt := fmt.Sprint(retry.Attempt)
	if retry.MaxAttempts > 0 {
		attempt += fmt.Sprintf("/%d", retry.MaxAttempts)
	}
	text := fmt.Sprintf("Retrying · attempt %s in %.3gs · %s", attempt, float64(retry.DelayMilliseconds)/1000, retry.Reason)
	r.routeMu.RLock()
	session := r.Current()
	id, err := r.Store.Append(session, turn, actor, "status", "", false, struct {
		Type      string          `json:"type"`
		RequestID int64           `json:"request_id"`
		Retry     *provider.Retry `json:"retry"`
		Text      string          `json:"text"`
	}{"model_retry", request, retry, text})
	r.routeMu.RUnlock()
	if err != nil {
		return fmt.Errorf("record model retry: %w", err)
	}
	event := Event{Kind: "status", Actor: actor, SessionID: session, EntryID: id, RequestID: request, Text: text}
	if purpose != "naming" {
		backoff := *retry
		event.Retry = &backoff
	}
	r.emit(event)
	return nil
}
