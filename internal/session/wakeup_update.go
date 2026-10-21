package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ttc/internal/prompts"
	"ttc/internal/tool"
)

type wakeupUpdate struct {
	ID      *string `json:"wakeup_id,omitempty"`
	Name    *string `json:"name,omitempty"`
	Message *string `json:"message,omitempty"`
	At      *string `json:"at,omitempty"`
	Delay   *int    `json:"delay_seconds,omitempty"`
	Repeat  *int    `json:"repeat_seconds,omitempty"`
}

func validateWakeupUpdate(a wakeupUpdate) error {
	if a.ID == nil && a.Name == nil {
		return errors.New(prompts.WakeupCancelIdentity)
	}
	if a.ID != nil {
		if err := tool.Required("wakeup_id", *a.ID); err != nil {
			return err
		}
	}
	if a.Name != nil {
		if err := tool.Required("name", *a.Name); err != nil {
			return err
		}
	}
	if a.Message == nil && a.At == nil && a.Delay == nil && a.Repeat == nil {
		return errors.New(prompts.WakeupUpdateEmpty)
	}
	if a.Message != nil {
		if err := tool.Required("message", *a.Message); err != nil {
			return err
		}
	}
	if a.At != nil && a.Delay != nil {
		return errors.New(prompts.WakeupUpdateTime)
	}
	if a.At != nil {
		if _, err := time.Parse(time.RFC3339, *a.At); err != nil {
			return fmt.Errorf(prompts.WakeupInvalidTimestamp, err)
		}
	}
	if a.Delay != nil && (*a.Delay < 0 || *a.Delay > maxWakeupSeconds) {
		return errors.New(prompts.WakeupDelayRange)
	}
	if a.Repeat != nil && (*a.Repeat < 0 || *a.Repeat > maxWakeupSeconds) {
		return errors.New(prompts.WakeupUpdateRepeat)
	}
	return nil
}

// update changes only an active reminder. Its lock also orders committed firings:
// an update cannot retract an already published firing or change its payload.
func (w *wakeups) update(a wakeupUpdate, actor string) (wakeup, error) {
	if err := validateWakeupUpdate(a); err != nil {
		return wakeup{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return wakeup{}, errors.New(prompts.RuntimeEnded)
	}
	id, name := "", ""
	if a.ID != nil {
		id = *a.ID
	}
	if a.Name != nil {
		name = *a.Name
	}
	v := w.findActiveLocked(actor, id, name)
	if v == nil {
		return wakeup{}, tool.Fail("not_found", prompts.WakeupCancelNotFound)
	}
	if a.Message != nil {
		v.Message = *a.Message
	}
	if a.Repeat != nil {
		v.Repeat = *a.Repeat
	}
	if a.At != nil {
		v.nextAt, _ = time.Parse(time.RFC3339, *a.At)
	} else if a.Delay != nil {
		v.nextAt = time.Now().Add(time.Duration(*a.Delay) * time.Second)
	}
	v.NextAt = v.nextAt.UTC().Format(time.RFC3339)
	select {
	case v.changed <- struct{}{}:
	default:
	}
	snapshot := *v
	w.notify(snapshot)
	return snapshot, nil
}

func (r *Runtime) addWakeupUpdateTool() {
	tool.Register(r.Tools, "wakeup_update", prompts.ToolDescription("wakeup_update"), map[string]any{
		"wakeup_id": tool.Property("string"), "name": tool.Property("string"), "message": tool.Property("string"),
		"at": tool.Property("string"), "delay_seconds": tool.IntegerRange(0, maxWakeupSeconds), "repeat_seconds": tool.IntegerRange(0, maxWakeupSeconds),
	}, nil, validateWakeupUpdate, func(ctx context.Context, x tool.Execution, a wakeupUpdate) (any, error) {
		v, err := r.timers.update(a, x.Actor)
		if err != nil {
			return nil, err
		}
		return map[string]any{"wakeup_id": v.ID, "name": v.Name, "message": v.Message, "next_at": v.NextAt,
			"repeat_seconds": v.Repeat, "fired_count": v.Fired, "status": v.Status}, nil
	})
}
