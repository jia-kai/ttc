package llm

import (
	"fmt"

	"ttc/internal/prompts"
)

// NewCompactionContext prepends a continuation hint to a generated Markdown
// summary in a developer context message, preserving the summary verbatim.
func NewCompactionContext(summary string) Message {
	return Message{Type: TypeCompactionContext, Content: fmt.Sprintf(prompts.CompactionContext, summary)}
}
