package context

import "ttc/internal/llm"

// CompactionReplacement materializes a checked summary, already-selected
// retained human inputs and recent tail into canonical replacement input. It
// preserves message order and metadata, removes provider replay state, and adds
// pending only when that recovery instruction is not already present. Selection,
// token policy and persistence remain with the caller. The returned slice is
// independent; nested message data other than State remains shared and immutable.
func CompactionReplacement(handoff llm.Message, retained, tail []llm.Message, pending *llm.Message) []llm.Message {
	messages := make([]llm.Message, 0, 1+len(retained)+len(tail))
	messages = append(messages, handoff)
	messages = append(messages, retained...)
	messages = append(messages, tail...)
	messages, _ = AppendPendingMessage(messages, pending)
	for i := range messages {
		messages[i].State = nil
	}
	return messages
}
