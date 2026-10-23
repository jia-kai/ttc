package history

import (
	"encoding/json"

	"ttc/internal/llm"
)

// Session identifies the selected history/file tips. Zero tips mean an empty tree.
type Session struct {
	CompactionError                       string // Nonempty permanently disables inference in this context; history remains inspectable.
	ID, WorkspaceID, LineageID, Name      string
	ReadOnly                              bool
	EntryTip, FileTip, RedoTip, UndoFloor int64
	Model                                 llm.Selection
	LastActivityMS                        int64 // Unix milliseconds; date grouping uses the frontend local timezone.
}

// Entry is one immutable node in chronological history.
type Entry struct {
	MainTurnID, UndoOwnerTurnID          string // Chronological inference attribution and most recent human undo checkpoint.
	ID, Parent, Source, FileTip          int64
	SessionID, TurnID, Actor, Kind, Role string
	Visible                              bool
	Content                              json.RawMessage
	CreatedMS                            int64
}

// EventSeq returns the immutable semantic commit identity. Continuation copies
// preserve their original source; physical row IDs remain separate.
func (e Entry) EventSeq() int64 {
	if e.Source != 0 {
		return e.Source
	}
	return e.ID
}

// InternalEvent identifies ordering and delivery records already presented by
// messages or tool results. These remain inspectable but add no conversation row.
func (e Entry) InternalEvent() bool {
	if e.Kind != "status" {
		return false
	}
	var status struct{ Type string }
	if json.Unmarshal(e.Content, &status) != nil {
		return false
	}
	switch status.Type {
	case "presentation_stream", "request_admitted", "request_finished", "job_state", "timer_state", "child_state", "turn_finished", "runtime_event", "compaction_started", "compaction_recovery", "file_changed":
		return true
	}
	return false
}

// PromptMetadata identifies an instruction snapshot and its placeholder group.
// Purpose defaults to coding when no request is associated with the snapshot.
type PromptMetadata struct {
	Actor, Purpose, Path string
	RequestID            int64 // Zero when the snapshot has no associated request.
}

// GroupKey returns actor + NUL + purpose for one replay placeholder per group.
func (p PromptMetadata) GroupKey() string { return p.Actor + "\x00" + p.Purpose }

// EntryView is the typed, small presentation view used for history replay.
// Message is canonical for message/summary entries; status bodies stay unloaded.
// Prompt is nonnil only for system_prompt statuses. No instruction file or tool
// detail is loaded to produce a view.
type EntryView struct {
	Entry      Entry
	Label      string
	Message    *llm.Message
	StatusType string
	Prompt     *PromptMetadata
}

// Inspection contains complete small-inspector content and display metadata.
// Large records should first use InspectPage to avoid loading whole envelopes.
type Inspection struct {
	ID                int64
	Actor, Kind, Type string
	Title, Text       string
	System, Markdown  bool
	Structured        bool // Readable provider inspection generated from typed IR.
}
