package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// recoverPartial runs only after partial output and interrupted call outcomes
// have been persisted. It does not execute or replay tools. The returned runtime
// instruction stays pending until a coding request succeeds, including compaction.
func (r *Runtime) recoverPartial(ctx context.Context, turn, actor string, request int64, priorAttempts int, failure *llm.PartialError) (llm.Message, error) {
	if err := ctx.Err(); err != nil {
		return llm.Message{}, err
	}
	if failure == nil || failure.Err == nil || failure.Retry.Attempt-1 <= priorAttempts {
		return llm.Message{}, errors.New("provider emitted invalid partial recovery")
	}
	if err := validateRetry(&failure.Retry); err != nil {
		return llm.Message{}, err
	}
	message := llm.Message{Role: "developer", Runtime: true, RequestID: request, Content: prompts.Recovery}
	if _, err := r.ensureRecovery(turn, actor, message); err != nil {
		return llm.Message{}, err
	}
	if err := r.retryNotice(turn, actor, request, "coding", &failure.Retry); err != nil {
		return llm.Message{}, err
	}
	timer := time.NewTimer(time.Duration(failure.Retry.DelayMilliseconds) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return llm.Message{}, ctx.Err()
	case <-timer.C:
		return message, ctx.Err()
	}
}

// ensureRecovery persists one warning in the actor's active branch. Compaction
// owns model-input restoration; this only restores child durability if a main
// handoff archived its original entry. Recovery messages never enter main input
// when owned by children or asides.
func (r *Runtime) ensureRecovery(turn, actor string, message llm.Message) (bool, error) {
	r.routeMu.RLock()
	session := r.Current()
	entries, err := r.Store.Branch(session, 0)
	if err != nil {
		r.routeMu.RUnlock()
		return false, fmt.Errorf("read recovery instructions: %w", err)
	}
	for _, entry := range entries {
		if entry.Actor != actor || entry.Kind != "message" || entry.Role != message.Role {
			continue
		}
		var existing llm.Message
		if err := json.Unmarshal(entry.Content, &existing); err != nil {
			r.routeMu.RUnlock()
			return false, fmt.Errorf("read recovery instructions: %w", err)
		}
		if _, added := contextbuild.AppendPendingMessage([]llm.Message{existing}, &message); !added {
			r.routeMu.RUnlock()
			return false, nil
		}
	}
	id, err := r.Store.Append(session, turn, actor, "message", message.Role, actor == "main", message)
	r.routeMu.RUnlock()
	if err != nil {
		return false, fmt.Errorf("record recovery instructions: %w", err)
	}
	r.emit(Event{Kind: "message_placeholder", Actor: actor, SessionID: session, EntryID: id, Text: "Recovery instructions · inspect"})
	return true, nil
}

// retryNotice commits an inspectable system message without adding model input.
// Route locking keeps child notices in the active compaction continuation.
// Naming retries remain notices without replacing foreground UI activity.
func (r *Runtime) retryNotice(turn, actor string, request int64, purpose string, retry *llm.Retry) error {
	if err := validateRetry(retry); err != nil {
		return err
	}
	attempt := fmt.Sprint(retry.Attempt)
	if retry.MaxAttempts > 0 {
		attempt += fmt.Sprintf("/%d", retry.MaxAttempts)
	}
	text := fmt.Sprintf("Retrying · attempt %s in %.3gs · %s", attempt, float64(retry.DelayMilliseconds)/1000, retry.Reason)
	r.routeMu.RLock()
	session := r.Current()
	id, err := r.Store.Append(session, turn, actor, "status", "", false, struct {
		Type      string     `json:"type"`
		RequestID int64      `json:"request_id"`
		Retry     *llm.Retry `json:"retry"`
		Text      string     `json:"text"`
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

func validateRetry(retry *llm.Retry) error {
	if retry == nil || retry.Attempt < 2 || retry.MaxAttempts < 0 || retry.MaxAttempts > 0 && retry.Attempt > retry.MaxAttempts || retry.DelayMilliseconds < 0 || retry.DelayMilliseconds > int64((1<<63-1)/time.Millisecond) || retry.Reason == "" {
		return errors.New("provider emitted invalid retry notice")
	}
	return nil
}
