package context

import (
	"fmt"
	"sort"
	"time"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

func validateRetainedInput(message llm.Message) error {
	if message.Type != llm.TypeUserInput {
		return fmt.Errorf(prompts.ContextRetainedInputRole)
	}
	if _, err := inputSource(message); err != nil {
		return err
	}
	if message.InputTimeMS <= 0 || message.RetainedAtMS <= 0 {
		return fmt.Errorf(prompts.ContextRetainedInputTimestamps)
	}
	if message.InputTimeMS > message.RetainedAtMS {
		return fmt.Errorf(prompts.ContextRetainedInputFuture, message.InputTimeMS, message.RetainedAtMS)
	}
	return nil
}

// RetainedInputMessages copies selected human inputs with their compaction time
// in Unix milliseconds. Indices must be increasing, in bounds and at most four.
// Original text, source, commit time, attachments, identity and replay state are
// unchanged; canonical replay callers are responsible for excluding native State.
func RetainedInputMessages(messages []llm.Message, inputs []int, at time.Time) ([]llm.Message, error) {
	if len(inputs) > 4 {
		return nil, fmt.Errorf(prompts.ContextRetainedInputLimit)
	}
	retained := make([]llm.Message, 0, len(inputs))
	previous := -1
	for _, index := range inputs {
		if index <= previous || index < 0 || index >= len(messages) {
			return nil, fmt.Errorf(prompts.ContextRetainedInputIndex, index)
		}
		message := messages[index]
		message.RetainedAtMS = at.UnixMilli()
		if err := validateRetainedInput(message); err != nil {
			return nil, fmt.Errorf(prompts.ContextRetainedInputFailure, index, err)
		}
		retained = append(retained, message)
		previous = index
	}
	return retained, nil
}

// ValidateRetainedInputs checks typed retained metadata at persistence and
// compaction boundaries. Zero denotes an ordinary message; negative times,
// nonhuman inputs, invalid sources and missing or future original times fail.
func ValidateRetainedInputs(messages []llm.Message) error {
	for i, message := range messages {
		if message.RetainedAtMS != 0 {
			if err := validateRetainedInput(message); err != nil {
				return fmt.Errorf(prompts.ContextRetainedInputFailure, i, err)
			}
		}
	}
	return nil
}

// ProjectRetainedInputs replaces contiguous retained human messages with one
// structured user message wrapped in retained_inputs. Inputs sort by
// exact original time ascending with stable ties. Original expanded text,
// explicit input types and two-decimal relative times are preserved. Attachment
// indices identify occurrences in the combined native Files without deduplication.
// Numeric timestamps stay canonical-only; history and other messages are unchanged.
func ProjectRetainedInputs(messages []llm.Message) ([]llm.Message, error) {
	if err := ValidateRetainedInputs(messages); err != nil {
		return nil, err
	}
	first := -1
	for i, message := range messages {
		if message.RetainedAtMS > 0 {
			first = i
			break
		}
	}
	if first < 0 {
		return messages, nil
	}
	type entry struct {
		Type              string `yaml:"type"`
		Timestamp         string `yaml:"timestamp"`
		Content           string `yaml:"content"`
		AttachmentIndices []int  `yaml:"attachment_indices,omitempty"`
	}
	out := make([]llm.Message, 0, len(messages))
	out = append(out, messages[:first]...)
	for i := first; i < len(messages); {
		if messages[i].RetainedAtMS == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		start := i
		for i < len(messages) && messages[i].RetainedAtMS > 0 {
			i++
		}
		inputs := append([]llm.Message(nil), messages[start:i]...)
		sort.SliceStable(inputs, func(i, j int) bool { return inputs[i].InputTimeMS < inputs[j].InputTimeMS })
		group := struct {
			Inputs []entry `yaml:"inputs"`
		}{}
		user := llm.Message{Type: llm.TypeUserInput}
		for _, input := range inputs {
			source, _ := inputSource(input) // Validated above.
			var kind string
			switch source {
			case "normal":
				kind = "user_input"
			case "queue":
				kind = "queued_user_input"
			case "steer":
				kind = "user_steer"
			case "task":
				kind = "child_task"
			case "btw":
				kind = "aside_task"
			}
			item := entry{Type: kind, Timestamp: fmt.Sprintf(prompts.ContextRetainedTimestamp, float64(input.RetainedAtMS-input.InputTimeMS)/1000), Content: input.Content}
			for _, file := range input.Files {
				item.AttachmentIndices = append(item.AttachmentIndices, len(user.Files))
				user.Files = append(user.Files, file)
			}
			group.Inputs = append(group.Inputs, item)
		}
		content, err := llm.FormatModelData("retained_inputs", group)
		if err != nil {
			return nil, err
		}
		user.Content = content
		out = append(out, user)
	}
	return out, nil
}
