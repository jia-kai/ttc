package session

import (
	"errors"
	"fmt"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/render"
)

func pendingToolKey(request int64, id string) string { return fmt.Sprintf("stream:%d:%s", request, id) }
func streamState(err error) string {
	if err != nil {
		return "interrupted"
	}
	return "received"
}

// toolAnnouncement records inspectable display metadata, never an executable
// intent. Completed calls are committed only after the response stream settles.
func (r *Runtime) toolAnnouncement(turn, actor string, request int64, start *llm.ToolStart) error {
	if start == nil || start.ID == "" || start.Name == "" {
		return errors.New(prompts.SessionInvalidToolAnnouncement)
	}
	text := "Tool announced · " + render.Clean(start.Name)
	r.routeMu.RLock()
	session := r.Current()
	id, err := r.Store.Append(session, turn, actor, "status", "", false, struct {
		Type      string         `json:"type"`
		RequestID int64          `json:"request_id"`
		Call      *llm.ToolStart `json:"call"`
		Text      string         `json:"text"`
	}{"tool_stream", request, start, text})
	r.routeMu.RUnlock()
	if err != nil {
		return fmt.Errorf(prompts.SessionRecordToolAnnouncement, err)
	}
	text = "awaiting " + render.Clean(start.Name) + " (0seg/0bytes) ..."
	r.emit(Event{Kind: "tool_pending", Actor: actor, SessionID: session, EntryID: id, CallID: pendingToolKey(request, start.ID), Text: text})
	return nil
}

// toolProgress updates the live announcement without adding history entries.
func (r *Runtime) toolProgress(actor string, request int64, progress *llm.ToolProgress) error {
	if progress == nil || progress.ID == "" || progress.Name == "" || progress.Segments < 0 || progress.Bytes < 0 {
		return errors.New(prompts.SessionInvalidToolProgress)
	}
	text := fmt.Sprintf("awaiting %s (%dseg/%dbytes) ...", render.Clean(progress.Name), progress.Segments, progress.Bytes)
	r.emit(Event{Kind: "tool_progress", Actor: actor, CallID: pendingToolKey(request, progress.ID), Text: text})
	return nil
}
