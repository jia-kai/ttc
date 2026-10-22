package llm

import (
	"encoding/json"
	"fmt"

	"ttc/internal/prompts"
)

// NewCompactionContext frames a generated summary as quoted reference data in a
// TTC-authored developer context message, not an ordinary assistant utterance.
func NewCompactionContext(summary string) Message {
	// Marshaling a string cannot fail. JSON quoting prevents summary text from
	// closing a delimiter and escaping the TTC-authored context framing.
	quoted, _ := json.Marshal(summary)
	return Message{Type: TypeCompactionContext, Content: fmt.Sprintf(prompts.CompactionContext, quoted)}
}
