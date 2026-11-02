package llm

// NewCompactionContext records a generated Markdown summary as historical
// user-role context, preserving its text verbatim. The shared system prompt
// supplies continuation semantics; ModelContent adds its model-facing marker.
func NewCompactionContext(summary string) Message {
	return Message{Type: TypeCompactionContext, Content: summary}
}
