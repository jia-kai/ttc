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

// ModelContent projects steering into a receiver-facing envelope and marks
// runtime notifications. Canonical content and input metadata remain unchanged;
// human-authored JSON is never inspected to determine message semantics.
func (m Message) ModelContent() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	if m.Type == TypeChildGuidance {
		var guidance struct {
			Message       *string  `json:"message"`
			JobReadGrants []string `json:"job_read_grants,omitempty"`
		}
		if err := json.Unmarshal([]byte(m.Content), &guidance); err != nil {
			return "", fmt.Errorf(prompts.LLMInvalidChildSteer, err)
		}
		if guidance.Message == nil {
			return "", fmt.Errorf(prompts.LLMChildSteerMissingMessage)
		}
		return modelSteer("main", *guidance.Message, guidance.JobReadGrants)
	}
	if m.Type == TypeUserInput && m.InputSource == "steer" {
		return modelSteer("user", m.Content, nil)
	}
	if m.Type != TypeRuntimeNotification {
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

// modelSteer shares one wire shape for human and parent guidance. Routing and
// delivery bookkeeping stay canonical-only; grants describe new read access.
func modelSteer(source, message string, grants []string) (string, error) {
	data, err := json.Marshal(struct {
		Type          string   `json:"type"`
		Source        string   `json:"source"`
		Message       string   `json:"message"`
		JobReadGrants []string `json:"job_read_grants,omitempty"`
	}{Type: "steer", Source: source, Message: message, JobReadGrants: grants})
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
