package llm

import (
	"encoding/json"
	"fmt"

	"ttc/internal/prompts"
)

// MessageType identifies trusted message semantics, independently of model roles.
// Constructors assign types; message content never determines a type.
type MessageType string

const (
	TypeUserInput            MessageType = "user_input"
	TypeAssistant            MessageType = "assistant"
	TypeToolResult           MessageType = "tool_result"
	TypeSystemInstruction    MessageType = "system_instruction"
	TypeDeveloperInstruction MessageType = "developer_instruction"
	TypeRuntimeContext       MessageType = "runtime_context"
	TypeCompactionContext    MessageType = "compaction_context"
	TypeRuntimeNotification  MessageType = "runtime_notification"
	TypeRuntimeInstruction   MessageType = "runtime_instruction"
	TypeChildGuidance        MessageType = "child_guidance"
)

// PresentationCategory describes message presentation, not model authority.
type PresentationCategory string

const (
	PresentationUser         PresentationCategory = "user"
	PresentationAssistant    PresentationCategory = "assistant"
	PresentationTool         PresentationCategory = "tool"
	PresentationInstruction  PresentationCategory = "instruction"
	PresentationContext      PresentationCategory = "context"
	PresentationNotification PresentationCategory = "notification"
	PresentationGuidance     PresentationCategory = "guidance"
)

func (t MessageType) policy() (string, PresentationCategory) {
	switch t {
	case TypeUserInput:
		return "user", PresentationUser
	case TypeAssistant:
		return "assistant", PresentationAssistant
	case TypeToolResult:
		return "tool", PresentationTool
	case TypeSystemInstruction:
		return "system", PresentationInstruction
	case TypeDeveloperInstruction:
		return "developer", PresentationInstruction
	case TypeRuntimeContext, TypeCompactionContext:
		return "developer", PresentationContext
	case TypeRuntimeNotification:
		return "user", PresentationNotification
	case TypeRuntimeInstruction:
		return "developer", PresentationInstruction
	case TypeChildGuidance:
		return "user", PresentationGuidance
	default:
		panic(fmt.Sprintf("invalid message type %q", t))
	}
}

// Validate rejects unknown types at persistence and provider boundaries.
func (t MessageType) Validate() error {
	switch t {
	case TypeUserInput, TypeAssistant, TypeToolResult, TypeSystemInstruction, TypeDeveloperInstruction, TypeRuntimeContext, TypeCompactionContext, TypeRuntimeNotification, TypeRuntimeInstruction, TypeChildGuidance:
		return nil
	default:
		return fmt.Errorf(prompts.LLMInvalidMessageType, t)
	}
}

// IsInput reports whether this is an authored human input.
func (t MessageType) IsInput() bool { return t == TypeUserInput }

// IsRuntime reports whether TTC authored this message rather than a human/model.
func (t MessageType) IsRuntime() bool {
	return t == TypeRuntimeContext || t == TypeCompactionContext || t == TypeRuntimeNotification || t == TypeRuntimeInstruction || t == TypeChildGuidance
}

// Role returns the model-facing authority derived from the semantic type.
func (t MessageType) Role() string { role, _ := t.policy(); return role }

// Presentation returns the centrally assigned display category.
func (t MessageType) Presentation() PresentationCategory { _, category := t.policy(); return category }

// Role returns the derived model role; it is never separately persisted.
func (m Message) Role() string { return m.Type.Role() }

// ModelContent marks runtime notifications and child guidance without changing
// their payload type field. Human-authored JSON is never inspected or rewritten.
func (m Message) ModelContent() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	if m.Type != TypeRuntimeNotification && m.Type != TypeChildGuidance {
		return m.Content, nil
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal([]byte(m.Content), &event); err != nil || event == nil {
		// Text-only notices still receive an explicit envelope. Structured event
		// objects retain their original top-level discriminator and payload fields.
		payload, _ := json.Marshal(m.Content)
		event = map[string]json.RawMessage{"event": payload}
	}
	marker, _ := json.Marshal(string(m.Type))
	event["message_type"] = marker
	data, err := json.Marshal(event)
	return string(data), err
}

// MarshalJSON persists canonical semantics and rejects incompatible messages.
func (m Message) MarshalJSON() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	type plain Message
	return json.Marshal(plain(m))
}

// UnmarshalJSON accepts only the canonical schema. Legacy role/runtime decoding
// belongs exclusively to the versioned history migration boundary.
func (m *Message) UnmarshalJSON(data []byte) error {
	type plain Message
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, ok := fields["role"]; ok {
		return fmt.Errorf(prompts.LLMLegacyMessageRole)
	}
	if _, ok := fields["runtime"]; ok {
		return fmt.Errorf(prompts.LLMLegacyMessageRuntime)
	}
	candidate := Message(value)
	if err := candidate.Validate(); err != nil {
		return err
	}
	*m = candidate
	return nil
}

// Validate checks semantic compatibility without examining user content.
func (m Message) Validate() error {
	if err := m.Type.Validate(); err != nil {
		return err
	}
	if len(m.Calls) > 0 && m.Type != TypeAssistant {
		return fmt.Errorf(prompts.LLMMessageToolCalls, m.Type)
	}
	if m.CallID != "" && m.Type != TypeToolResult {
		return fmt.Errorf(prompts.LLMMessageCallID, m.Type)
	}
	if m.State != nil && m.Type != TypeAssistant {
		return fmt.Errorf(prompts.LLMMessageReplayState, m.Type)
	}
	if (m.UserText != nil || m.InputSource != "" || m.InputTimeMS != 0 || m.RetainedAtMS != 0) && m.Type != TypeUserInput {
		return fmt.Errorf(prompts.LLMMessageInputMetadata, m.Type)
	}
	return nil
}
