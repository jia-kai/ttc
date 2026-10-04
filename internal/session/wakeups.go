package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
	"ttc/internal/history"
	"ttc/internal/prompts"
	"ttc/internal/tool"
)

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
	deliveredCount int // Last admitted firing; later firings remain pending.
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
func (w *wakeups) schedule(name, message string, at time.Time, repeat int) (wakeup, error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return wakeup{}, errors.New("runtime ended")
	}
	for _, v := range w.items {
		if v.Name == name && v.Status == "scheduled" {
			w.mu.Unlock()
			return wakeup{}, errors.New("wakeup name already active; choose another name or cancel the existing reminder with wakeup_cancel first")
		}
	}
	ctx, cancel := context.WithCancel(w.ctx)
	v := &wakeup{ID: history.NewID("wake"), Name: name, Message: message, Status: "scheduled", NextAt: at.UTC().Format(time.RFC3339), Repeat: repeat, cancel: cancel}
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
			} else {
				next = time.Now().Add(time.Duration(repeat) * time.Second)
				v.NextAt = next.UTC().Format(time.RFC3339)
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
	type schedule struct {
		Name    string  `json:"name"`
		Message string  `json:"message"`
		At      *string `json:"at,omitempty"`
		Delay   *int    `json:"delay_seconds,omitempty"`
		Repeat  *int    `json:"repeat_seconds,omitempty"`
	}
	tool.Register(r.Tools, "wakeup_schedule", prompts.ToolDescription("wakeup_schedule"), map[string]any{"name": tool.Property("string"), "message": tool.Property("string"), "at": tool.Property("string"), "delay_seconds": tool.Property("integer"), "repeat_seconds": tool.Property("integer")}, []string{"name", "message"}, func(a schedule) error {
		if e := tool.Required("name", a.Name); e != nil {
			return e
		}
		if e := tool.Required("message", a.Message); e != nil {
			return e
		}
		if (a.At == nil) == (a.Delay == nil) {
			return errors.New("exactly one of at or delay_seconds required")
		}
		if a.Delay != nil && (*a.Delay < 0 || *a.Delay > 31536000) {
			return errors.New("delay_seconds must be 0–31536000")
		}
		if a.Repeat != nil && (*a.Repeat < 1 || *a.Repeat > 31536000) {
			return errors.New("repeat_seconds must be 1–31536000 (omit for a one-shot timer)")
		}
		if a.At != nil {
			if _, e := time.Parse(time.RFC3339, *a.At); e != nil {
				return fmt.Errorf("at must be RFC3339 with a timezone, such as 2026-10-02T15:04:05Z; use delay_seconds instead for a relative time: %w", e)
			}
		}
		return nil
	}, func(ctx context.Context, x tool.Execution, a schedule) (any, error) {
		at := time.Now()
		if a.At != nil {
			at, _ = time.Parse(time.RFC3339, *a.At)
		} else {
			at = at.Add(time.Duration(*a.Delay) * time.Second)
		}
		repeat := 0
		if a.Repeat != nil {
			repeat = *a.Repeat
		}
		v, e := r.timers.schedule(a.Name, a.Message, at, repeat)
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
			return errors.New("exactly one of wakeup_id or name required")
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
			return nil, tool.Fail("not_found", "no scheduled reminder matches; use wakeup_list and choose an active wakeup_id or name")
		}
		return map[string]any{"wakeup_id": v.ID, "status": v.Status}, nil
	})
}
