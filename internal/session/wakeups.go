package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
	"ttc/internal/history"
	"ttc/internal/prompts"
	"ttc/internal/render"
	"ttc/internal/tool"
)

type wakeupSchedule struct {
	Name    string  `json:"name"`
	Message string  `json:"message"`
	At      *string `json:"at,omitempty"`
	Delay   *int    `json:"delay_seconds,omitempty"`
	Repeat  *int    `json:"repeat_seconds,omitempty"`
}

type wakeup struct {
	ID             string `json:"wakeup_id"`
	Name           string `json:"name"`
	Message        string `json:"message"`
	Status         string `json:"status"`
	NextAt         string `json:"next_at,omitempty"`
	Repeat         int    `json:"repeat_seconds,omitempty"`
	Fired          int    `json:"fired_count"`
	Last           string `json:"last_result,omitempty"`
	cancel         context.CancelFunc
	actor          string    // Immutable scheduler actor ID; names are resolved by the frontend.
	startupText    string    // Immutable startup Markdown, preserving original option omission.
	nextAt         time.Time // Exact next deadline, without the display timestamp's second rounding.
	deliveredCount int       // Last admitted firing; later firings remain pending.
}
type wakeups struct {
	ctx    context.Context
	mu     sync.Mutex
	items  map[string]*wakeup
	notify func(wakeup)
	wg     sync.WaitGroup
	closed bool
}

func newWakeups(ctx context.Context, notify func(wakeup)) *wakeups {
	return &wakeups{ctx: ctx, items: map[string]*wakeup{}, notify: notify}
}
func (w *wakeups) schedule(a wakeupSchedule, actor string, at time.Time) (wakeup, error) {
	// Build the immutable inspection text before publication, independent of the
	// decoded call's optional-argument pointers and later inspection refreshes.
	startup, err := json.Marshal(a)
	if err != nil {
		return wakeup{}, fmt.Errorf(prompts.WakeupEncodeStartup, err)
	}
	startupText := "### Original startup parameters\n\n" + render.Fence(string(startup), "json")
	repeat := 0
	if a.Repeat != nil {
		repeat = *a.Repeat
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return wakeup{}, errors.New(prompts.RuntimeEnded)
	}
	for _, v := range w.items {
		if v.Name == a.Name && v.Status == "scheduled" {
			w.mu.Unlock()
			return wakeup{}, errors.New(prompts.WakeupNameActive)
		}
	}
	ctx, cancel := context.WithCancel(w.ctx)
	v := &wakeup{ID: history.NewID("wake"), Name: a.Name, Message: a.Message, Status: "scheduled", NextAt: at.UTC().Format(time.RFC3339), Repeat: repeat, cancel: cancel, actor: actor, startupText: startupText, nextAt: at}
	w.items[v.ID] = v
	w.wg.Add(1)
	initial := *v
	w.notify(initial)
	w.mu.Unlock()
	go func() {
		defer w.wg.Done()
		next := at
		for {
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			w.mu.Lock()
			if w.closed || v.Status == "cancelled" {
				w.mu.Unlock()
				return
			}
			v.Fired++
			if repeat == 0 {
				v.Status = "fired"
				v.NextAt = ""
				v.nextAt = time.Time{}
			} else {
				next = time.Now().Add(time.Duration(repeat) * time.Second)
				v.NextAt = next.UTC().Format(time.RFC3339)
				v.nextAt = next
			}
			snapshot := *v
			w.notify(snapshot)
			w.mu.Unlock()
			if repeat == 0 {
				return
			}
		}
	}()
	return initial, nil
}
func (w *wakeups) list() []wakeup {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]wakeup, 0, len(w.items))
	for _, v := range w.items {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (w *wakeups) deliveredThrough(id string, fired int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if v := w.items[id]; v != nil {
		if fired > v.deliveredCount {
			v.deliveredCount = fired
		}
		if v.deliveredCount >= v.Fired {
			v.Last = "delivered"
		}
	}
}
func (w *wakeups) stop(id, name string) (wakeup, error) {
	w.mu.Lock()
	for _, v := range w.items {
		if (id != "" && v.ID == id) || (name != "" && v.Name == name) {
			if v.Status != "scheduled" {
				continue
			}
			v.Status = "cancelled"
			v.NextAt = ""
			v.nextAt = time.Time{}
			v.cancel()
			snapshot := *v
			w.notify(snapshot)
			w.mu.Unlock()
			return snapshot, nil
		}
	}
	w.mu.Unlock()
	return wakeup{}, errors.New("not_found")
}
func (w *wakeups) close() {
	w.mu.Lock()
	w.closed = true
	for _, v := range w.items {
		v.cancel()
	}
	w.mu.Unlock()
	w.wg.Wait()
	w.mu.Lock()
	w.items = map[string]*wakeup{}
	w.mu.Unlock()
}
func (r *Runtime) addWakeupTools() {
	tool.Register(r.Tools, "wakeup_schedule", prompts.ToolDescription("wakeup_schedule"), map[string]any{"name": tool.Property("string"), "message": tool.Property("string"), "at": tool.Property("string"), "delay_seconds": tool.Property("integer"), "repeat_seconds": tool.Property("integer")}, []string{"name", "message"}, func(a wakeupSchedule) error {
		if e := tool.Required("name", a.Name); e != nil {
			return e
		}
		if e := tool.Required("message", a.Message); e != nil {
			return e
		}
		if (a.At == nil) == (a.Delay == nil) {
			return errors.New(prompts.WakeupTimeRequired)
		}
		if a.Delay != nil && (*a.Delay < 0 || *a.Delay > 31536000) {
			return errors.New(prompts.WakeupDelayRange)
		}
		if a.Repeat != nil && (*a.Repeat < 1 || *a.Repeat > 31536000) {
			return errors.New(prompts.WakeupRepeatRange)
		}
		if a.At != nil {
			if _, e := time.Parse(time.RFC3339, *a.At); e != nil {
				return fmt.Errorf(prompts.WakeupInvalidTimestamp, e)
			}
		}
		return nil
	}, func(ctx context.Context, x tool.Execution, a wakeupSchedule) (any, error) {
		at := time.Now()
		if a.At != nil {
			at, _ = time.Parse(time.RFC3339, *a.At)
		} else {
			at = at.Add(time.Duration(*a.Delay) * time.Second)
		}
		v, e := r.timers.schedule(a, x.Actor, at)
		if e != nil {
			return nil, e
		}
		return map[string]any{"wakeup_id": v.ID, "name": v.Name, "next_at": v.NextAt, "repeat_seconds": v.Repeat, "status": v.Status}, nil
	})
	type empty struct{}
	tool.Register(r.Tools, "wakeup_list", prompts.ToolDescription("wakeup_list"), nil, nil, func(empty) error { return nil }, func(ctx context.Context, x tool.Execution, a empty) (any, error) {
		return map[string]any{"wakeups": r.timers.list()}, nil
	})
	type stop struct {
		ID   *string `json:"wakeup_id,omitempty"`
		Name *string `json:"name,omitempty"`
	}
	tool.Register(r.Tools, "wakeup_cancel", prompts.ToolDescription("wakeup_cancel"), map[string]any{"wakeup_id": tool.Property("string"), "name": tool.Property("string")}, nil, func(a stop) error {
		if (a.ID == nil) == (a.Name == nil) {
			return errors.New(prompts.WakeupCancelIdentity)
		}
		return nil
	}, func(ctx context.Context, x tool.Execution, a stop) (any, error) {
		id, name := "", ""
		if a.ID != nil {
			id = *a.ID
		}
		if a.Name != nil {
			name = *a.Name
		}
		v, e := r.timers.stop(id, name)
		if e != nil {
			return nil, tool.Fail("not_found", prompts.WakeupCancelNotFound)
		}
		return map[string]any{"wakeup_id": v.ID, "status": v.Status}, nil
	})
}
