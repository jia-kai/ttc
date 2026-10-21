package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/tool"
)

var errNeedsCompaction = errors.New("request requires compaction")

// deliverNoticeEntries updates timer delivery state only after durable request
// admission. It runs outside orderMu because timer publishers hold timers.mu
// before entering the publication gate.
func (r *Runtime) deliverNoticeEntries(actor string, entries []int64) error {
	for _, id := range entries {
		entry, err := r.Store.Entry(id)
		if err != nil {
			return err
		}
		var message llm.Message
		if err := json.Unmarshal(entry.Content, &message); err != nil {
			return err
		}
		var wake struct {
			ID    string `json:"wakeup_id"`
			Fired int    `json:"fired_count"`
		}
		if json.Unmarshal([]byte(message.Content), &wake) == nil && wake.ID != "" {
			r.timers.deliveredThrough(wake.ID, wake.Fired)
		}
		r.emit(Event{Kind: "message", Actor: actor, Text: message.Content, EntryID: id, MessageType: message.Type})
	}
	return nil
}

// queueNotification commits the source event before making its immutable body
// eligible for a request. Repeating timers replace only undelivered firings.
func (r *Runtime) queueNotification(content string) error {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	return r.queueNotificationLocked(content)
}
func (r *Runtime) queueNotificationLocked(content string) error {
	return r.queueActorNotificationLocked("main", content)
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
		if cm != nil {
			messages = append(messages, *cm)
		}
		projected, e := contextbuild.ProjectRetainedInputs(messages)
		if e != nil {
			return e
		}
		defs := r.Tools.Definitions()
		snapshot := contextbuild.InputSnapshot{Selection: selection, System: systemTemplate, Tools: defs, Messages: projected, Lineage: r.Current(), ReplayEstimator: r.Provider}
		observation := r.estimator.Prepare(snapshot)
		result := observation.Result
		r.mu.Lock()
		r.usage = usageFromEstimate(selection, result)
		r.mu.Unlock()
		if !contextbuild.FitsInput(result.Input, selection.Model.Budget, false) {
			return errNeedsCompaction
		}
		admitted, e = r.Store.AdmitRequest(r.Current(), turn, "main", selection, cm, r.notifications, nil, steers...)
		if e != nil {
			return e
		}
		r.notifications = nil
		r.steers = nil
		r.mainContext = next
		r.estimator.ObserveRequest(admitted.RequestID, observation)
		return nil
	})
	return admitted, cm, err
}

func (r *Runtime) checkContext() error {
	r.mu.Lock()
	fatal := r.fatalCompaction
	r.mu.Unlock()
	if fatal != "" {
		return fmt.Errorf(prompts.SessionUnusableAfterCompaction, fatal)
	}
	v, err := r.CurrentSession()
	if err != nil {
		return err
	}
	if v.CompactionError != "" {
		return fmt.Errorf(prompts.SessionUnusableAfterCompaction, v.CompactionError)
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
		if r.queueActorNotificationLocked(v.actor, string(content)) != nil {
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
		return errors.New(prompts.SessionNotificationSource)
	}
	if !json.Valid([]byte(content)) {
		return errors.New(prompts.SessionNotificationJSON)
	}
	for _, m := range r.notifications {
		if m.EventSeq == eventID {
			return errors.New(prompts.SessionEventAlreadyPending)
		}
	}
	r.notifications = append(r.notifications, llm.Message{Type: llm.TypeRuntimeNotification, Content: eventContent(content, eventID, 0), EventSeq: eventID})
	r.signalActorLocked("main")
	sort.Slice(r.notifications, func(i, j int) bool { return r.notifications[i].EventSeq < r.notifications[j].EventSeq })
	return nil
}

func (r *Runtime) publishChild(v tool.ChildView) error {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	return r.publishChildLocked(v)
}
func (r *Runtime) publishChildLocked(v tool.ChildView) error {
	_, err := r.Store.RecordChildState(r.Current(), history.ChildSnapshot{
		ChildID: v.ID, Label: v.Label, ContextState: v.ChildState,
		Outcome: v.AssignmentStatus, TurnID: v.TurnID, JobID: v.JobID,
	})
	if err != nil {
		r.orderError = errors.Join(r.orderError, err)
		return err
	}
	if v.ChildState == "closed" {
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

// eventContent applies trusted delivery identity without changing payload number
// precision. A positive recoveredFrom replaces any historical body provenance.
func eventContent(content string, id, recoveredFrom int64) string {
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &value) != nil || value == nil {
		return content
	}
	value["event_seq"] = json.RawMessage(strconv.FormatInt(id, 10))
	if recoveredFrom > 0 {
		value["recovered_from_event_seq"] = json.RawMessage(strconv.FormatInt(recoveredFrom, 10))
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return content
	}
	return string(encoded)
}
