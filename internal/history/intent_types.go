package history

import (
	"encoding/json"

	"ttc/internal/llm"
)

// ToolExecution identifies an admitted tool intent and the model capabilities
// frozen at its owning request. EntryID is the latest physical intent copy,
// including archived sessions; call IDs are globally unique.
type ToolExecution struct {
	EntryID   int64
	RequestID int64
	Selection llm.Selection
}

// ToolOutcome is saved tool identity and result data, without presentation.
// A nil Result means the tool has not completed; JSON null is a non-nil result.
type ToolOutcome struct {
	CallID string
	Name   string
	Result json.RawMessage
}
