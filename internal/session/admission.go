package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

var errNeedsCompaction = errors.New("request requires compaction")

// queueNotification commits the source event before making its immutable body
// eligible for a request. Repeating timers replace only undelivered firings.
func (r *Runtime) queueNotification(content string) error {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	return r.queueNotificationLocked(content)
}
func (r *Runtime) queueNotificationLocked(content string) error {
	id, err := r.Store.Append(r.Current(), "", "main", "status", "", false, map[string]any{"type": "runtime_event", "body": json.RawMessage(content)})
	if err != nil {
		r.orderError = errors.Join(r.orderError, err)
		return err
	}
	m := llm.Message{Role: "user", Runtime: true, Content: eventContent(content, id), EventSeq: id}
	var incoming struct {
		ID string `json:"wakeup_id"`
	}
	if json.Unmarshal([]byte(content), &incoming) == nil && incoming.ID != "" {
		for i, old := range r.notifications {
			var prior struct {
				ID string `json:"wakeup_id"`
			}
			if json.Unmarshal([]byte(old.Content), &prior) == nil && prior.ID == incoming.ID {
				r.notifications = append(r.notifications[:i], r.notifications[i+1:]...)
				break
			}
		}
	}
	r.notifications = append(r.notifications, m)
	return nil
}
func (r *Runtime) publishJob(v jobs.Snapshot) {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	v.Stdout, v.Stderr = "", "" // Captures are inspected separately, never copied into context.
	_, err := r.Store.Append(r.Current(), "", v.Owner, "status", "", false, map[string]any{"type": "job_state", "job": v})
	if err != nil {
		r.orderError = errors.Join(r.orderError, err)
		return
	}
	r.publishedJobs[v.ID] = v
}
func (r *Runtime) committedJobs(actor string) []jobs.Snapshot {
	// Caller owns orderMu, which also guards request admission.
	out := []jobs.Snapshot{}
	for _, v := range r.publishedJobs {
		if v.Owner == actor || strings.HasPrefix(v.Owner, actor+"/") {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// admitMain freezes delivery and live state under the same publication gate.
// A failed/oversized admission neither consumes notices nor advances cursors.
func (r *Runtime) admitMain(ctx context.Context, turn string, selection llm.Selection) (admitted history.Admission, cm *llm.Message, err error) {
	err = r.Workspace.Admit(ctx, func() error {
		r.orderMu.Lock()
		defer r.orderMu.Unlock()
		if r.orderError != nil {
			return fmt.Errorf("runtime event persistence failed: %w", r.orderError)
		}
		var next contextCursor
		var e error
		cm, next, e = r.runtimeContextLocked(ctx, "main", selection, r.mainContext)
		if e != nil {
			return e
		}
		messages, e := r.Store.Messages(r.Current())
		if e != nil {
			return e
		}
		messages = append(messages, r.notifications...)
		steers := r.steeringMessagesLocked()
		messages = append(messages, steers...)
		messages = r.contextMessages(selection, messages)
		if cm != nil {
			messages = append(messages, *cm)
		}
		defs := r.Tools.Definitions()
		r.mu.Lock()
		r.usage = estimateUsage(selection, systemTemplate, defs, messages)
		r.mu.Unlock()
		if !contextbuild.Fits(selection, systemTemplate, defs, messages, false) {
			return errNeedsCompaction
		}
		admitted, e = r.Store.AdmitRequest(r.Current(), turn, "main", selection, cm, r.notifications, nil, steers...)
		if e != nil {
			return e
		}
		r.notifications = nil
		r.steers = nil
		r.mainContext = next
		r.mu.Lock()
		r.usage.RequestID = admitted.RequestID
		r.mu.Unlock()
		return nil
	})
	return admitted, cm, err
}

func (r *Runtime) checkContext() error {
	r.mu.Lock()
	fatal := r.fatalCompaction
	r.mu.Unlock()
	if fatal != "" {
		return fmt.Errorf("session unusable after compaction: %s; inspect/export history or start/load another session", fatal)
	}
	v, err := r.CurrentSession()
	if err != nil {
		return err
	}
	if v.CompactionError != "" {
		return fmt.Errorf("session unusable after compaction: %s; inspect/export history or start/load another session", v.CompactionError)
	}
	return nil
}

func (r *Runtime) publishTimer(v wakeup) {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	_, err := r.Store.Append(r.Current(), "", "main", "status", "", false, map[string]any{"type": "timer_state", "timer": v})
	if err != nil {
		r.orderError = errors.Join(r.orderError, err)
		return
	}
	old := r.publishedTimers[v.ID]
	r.publishedTimers[v.ID] = v
	if v.Fired > old.Fired {
		content, _ := json.Marshal(map[string]any{"type": "wakeup", "wakeup_id": v.ID, "message": v.Message, "fired_count": v.Fired})
		if r.queueNotificationLocked(string(content)) != nil {
			return
		}
	}
}
func (r *Runtime) committedTimers() []wakeup {
	out := make([]wakeup, 0, len(r.publishedTimers))
	for _, v := range r.publishedTimers {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// queueCommittedNotification makes an existing semantic event eligible for
// parent delivery. Finish publication and the child's idle transition serialize
// under childStartMu; this gate prevents a request from consuming a partial event.
func (r *Runtime) queueCommittedNotification(content string, eventID int64) error {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	return r.queueCommittedNotificationLocked(content, eventID)
}
func (r *Runtime) queueCommittedNotificationLocked(content string, eventID int64) error {
	entry, err := r.Store.Entry(eventID)
	if err != nil {
		return err
	}
	if entry.EventSeq() != eventID {
		return errors.New("notification source must be an original committed event")
	}
	if !json.Valid([]byte(content)) {
		return errors.New("notification must contain valid JSON")
	}
	for _, m := range r.notifications {
		if m.EventSeq == eventID {
			return errors.New("event already pending delivery")
		}
	}
	r.notifications = append(r.notifications, llm.Message{Role: "user", Runtime: true, Content: eventContent(content, eventID), EventSeq: eventID})
	sort.Slice(r.notifications, func(i, j int) bool { return r.notifications[i].EventSeq < r.notifications[j].EventSeq })
	return nil
}

func (r *Runtime) publishChild(v tool.ChildView) error {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	return r.publishChildLocked(v)
}
func (r *Runtime) publishChildLocked(v tool.ChildView) error {
	_, err := r.Store.Append(r.Current(), v.TurnID, v.ID, "status", "", false, map[string]any{"type": "child_state", "child": v})
	if err != nil {
		r.orderError = errors.Join(r.orderError, err)
		return err
	}
	if v.State == "closed" {
		delete(r.publishedChildren, v.ID)
	} else {
		r.publishedChildren[v.ID] = v
	}
	return nil
}
func (r *Runtime) committedChildren(actor string) []tool.ChildView {
	out := []tool.ChildView{}
	for _, v := range r.publishedChildren {
		if actor == "main" || actor == v.ID {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func eventContent(content string, id int64) string {
	var value map[string]any
	if json.Unmarshal([]byte(content), &value) != nil || value == nil {
		return content
	}
	value["event_seq"] = id
	encoded, err := json.Marshal(value)
	if err != nil {
		return content
	}
	return string(encoded)
}
