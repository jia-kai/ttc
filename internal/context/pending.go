package context

import "ttc/internal/llm"

// AppendPendingMessage includes a required pending instruction unless its exact
// runtime/role/content/request identity is already present. It does not modify
// the input slice or message; the bool reports whether a message was appended.
func AppendPendingMessage(messages []llm.Message, pending *llm.Message) ([]llm.Message, bool) {
	if pending == nil {
		return messages, false
	}
	for _, message := range messages {
		if message.Runtime == pending.Runtime && message.Role == pending.Role && message.Content == pending.Content && message.RequestID == pending.RequestID {
			return messages, false
		}
	}
	result := make([]llm.Message, len(messages), len(messages)+1)
	copy(result, messages)
	return append(result, *pending), true
}
