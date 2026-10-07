package context

import (
	"encoding/json"
	"fmt"
	"time"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// InputMarker describes the following retained human message without changing
// it. The marker is historical context, not live runtime state, so manual loads
// preserve it. Commit and compaction timestamps are Unix milliseconds; age is elapsed
// milliseconds measured from the original commit, including after recompaction.
// Missing/nonpositive times, future commit times, invalid sources and nonhuman
// messages are errors. Native replay state remains the caller's responsibility.
func InputMarker(message llm.Message, compactedAt time.Time) (llm.Message, error) {
	if message.Role != "user" || message.Runtime {
		return llm.Message{}, fmt.Errorf("retained input must be a non-runtime user message")
	}
	source, err := inputSource(message)
	if err != nil {
		return llm.Message{}, err
	}
	at := compactedAt.UnixMilli()
	if message.InputTimeMS <= 0 || at <= 0 {
		return llm.Message{}, fmt.Errorf("retained input requires positive commit and compaction timestamps")
	}
	if message.InputTimeMS > at {
		return llm.Message{}, fmt.Errorf("retained input commit timestamp %d is after compaction timestamp %d", message.InputTimeMS, at)
	}
	metadata := struct {
		Type                  string `json:"type"`
		Source                string `json:"source"`
		OriginalCommittedAtMS int64  `json:"original_committed_at_ms"`
		CompactionAtMS        int64  `json:"compaction_at_ms"`
		AgeMS                 int64  `json:"age_ms"`
	}{"retained_input", source, message.InputTimeMS, at, at - message.InputTimeMS}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return llm.Message{}, fmt.Errorf("encode retained input metadata: %w", err)
	}
	return llm.Message{Role: "developer", Content: prompts.RetainedInput + "\n" + string(encoded)}, nil
}

// RetainedInputMessages interleaves metadata markers with the exact original
// selected human messages. Indices must be strictly increasing, in bounds and
// at most four. It never mutates messages or their text, images or replay state;
// callers preparing canonical replay are responsible for excluding native State.
func RetainedInputMessages(messages []llm.Message, inputs []int, at time.Time) ([]llm.Message, error) {
	if len(inputs) > 4 {
		return nil, fmt.Errorf("retained input selection exceeds four messages")
	}
	retained := make([]llm.Message, 0, 2*len(inputs))
	previous := -1
	for _, index := range inputs {
		if index <= previous || index < 0 || index >= len(messages) {
			return nil, fmt.Errorf("invalid retained input index %d: indices must be ordered and in bounds", index)
		}
		marker, err := InputMarker(messages[index], at)
		if err != nil {
			return nil, fmt.Errorf("retained input %d: %w", index, err)
		}
		retained = append(retained, marker, messages[index])
		previous = index
	}
	return retained, nil
}
