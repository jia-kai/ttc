package session

import (
	"errors"
	"fmt"
	"scicode/internal/provider"
	"scicode/internal/render"
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
func (r *Runtime) toolAnnouncement(turn, actor string, request int64, start *provider.ToolStart) error {
	if start == nil || start.ID == "" || start.Name == "" {
		return errors.New("provider emitted invalid tool announcement")
	}
	text := "Tool announced · " + render.Clean(start.Name)
	r.routeMu.RLock()
	session := r.Current()
	id, err := r.Store.Append(session, turn, actor, "status", "", false, struct {
		Type      string              `json:"type"`
		RequestID int64               `json:"request_id"`
		Call      *provider.ToolStart `json:"call"`
		Text      string              `json:"text"`
	}{"tool_stream", request, start, text})
	r.routeMu.RUnlock()
	if err != nil {
		return fmt.Errorf("record tool announcement: %w", err)
	}
	text = "awaiting " + render.Clean(start.Name) + " ..."
	r.emit(Event{Kind: "tool_pending", Actor: actor, SessionID: session, EntryID: id, CallID: pendingToolKey(request, start.ID), Text: text})
	return nil
}
