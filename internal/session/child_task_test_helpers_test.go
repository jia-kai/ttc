package session

import (
	"ttc/internal/llm"
	"ttc/internal/tool"
)

// childModelInput builds the full model-facing request for an isolated task.
// task.tools remains the separately scoped execution registry.
func childModelInput(actor string, selection llm.Selection, tools *tool.Registry, messages []llm.Message, aside bool) llm.Request {
	system := childSystemTemplate
	if aside {
		system = systemTemplate
	}
	return llm.Request{
		ConversationID: actor,
		Selection:      selection,
		System:         system,
		Tools:          tools.Definitions(),
		Messages:       append([]llm.Message{}, messages...),
		OutputTokens:   selection.Model.Budget.OutputAllowance,
	}
}
