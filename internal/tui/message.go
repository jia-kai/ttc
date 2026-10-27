package tui

import (
	"encoding/json"
	"strings"
	"time"

	"ttc/internal/llm"
	"ttc/internal/render"
)

// messageLine is the shared live/replay projection of a canonical message.
// Its caller applies actor decoration after projection, since child completion
// envelopes attribute their answer to the producing child rather than the receiver.
func messageLine(item line, message llm.Message) line {
	item.text = message.DisplayText()
	item.human, item.system, item.markdown, item.brief = false, false, false, false
	item.turnTime = ""
	switch message.Type.Presentation() {
	case llm.PresentationUser:
		item.text = "user · " + item.text
		item.human = true
		if (message.InputSource == "" || message.InputSource == "normal") && message.InputTimeMS > 0 {
			item.turnTime = time.UnixMilli(message.InputTimeMS).Local().Format("15:04")
		}
	case llm.PresentationAssistant:
		item.speaker, item.markdown, item.complete = "assistant", true, true
		item.requestID = message.RequestID
	case llm.PresentationTool:
		item.markdown, item.brief = true, true
	case llm.PresentationInstruction:
		item.text, item.system = "Instructions · inspect", true
		if message.Type == llm.TypeRuntimeInstruction {
			item.text = "Runtime instructions · inspect"
		}
	case llm.PresentationContext:
		item.text, item.system = "Runtime context · inspect", true
		if message.Type == llm.TypeCompactionContext {
			item.text = "Compaction context · inspect"
		}
	case llm.PresentationNotification:
		item = notificationLine(item, message.Content)
	case llm.PresentationGuidance:
		item.text = "Main-agent guidance · " + render.ResponsePrefix(message.Content)
		item.system = true
	}
	return item
}

// notificationLine summarizes an already typed notification. It is never called
// on human text: a JSON payload cannot grant itself runtime presentation.
func notificationLine(item line, content string) line {
	var notice struct {
		Type    string `json:"type"`
		ChildID string `json:"child_id"`
		JobID   string `json:"job_id"`
		Kind    string `json:"kind"`
		Label   string `json:"label"`
		Status  string `json:"status"`
		Name    string `json:"name"`
		Message string `json:"message"`
		Answer  string `json:"answer"`
	}
	if json.Unmarshal([]byte(content), &notice) != nil {
		item.text = "Runtime notification · " + render.ResponsePrefix(content)
		item.system = true
		return item
	}
	if notice.Type == "child_turn_finished" && strings.HasPrefix(notice.ChildID, "main/child_") {
		item.actor = notice.ChildID
		item.text, item.speaker, item.compactResponse = notice.Answer, "final answer", true
		return item
	}
	parts := []string{"Runtime notification"}
	if notice.Type != "" {
		parts = append(parts, strings.ReplaceAll(notice.Type, "_", " "))
	}
	job := ""
	if notice.JobID != "" {
		job = render.JobReference(notice.Kind, notice.Label, notice.JobID)
	}
	for _, field := range []string{job, notice.Name, notice.Status, notice.Message} {
		if field != "" {
			parts = append(parts, field)
		}
	}
	item.text = render.ResponsePrefix(strings.Join(parts, " · "))
	item.system, item.brief = true, true
	return item
}
