package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
	"ttc/internal/prompts"
)

// ModelContent projects typed canonical messages into the model-facing text
// schema without changing stored content, roles, attachments or provenance.
// Ordinary authored text is never inspected to infer a message classification.
func (m Message) ModelContent() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	switch m.Type {
	case TypeUserInput:
		if m.InputSource == "steer" {
			return modelText("steer", m.Content), nil
		}
	case TypeChildGuidance:
		var guidance struct {
			Message       *string  `json:"message"`
			JobReadGrants []string `json:"job_read_grants"`
		}
		if err := json.Unmarshal([]byte(m.Content), &guidance); err != nil {
			return "", fmt.Errorf(prompts.LLMInvalidChildSteer, err)
		}
		if guidance.Message == nil {
			return "", fmt.Errorf(prompts.LLMChildSteerMissingMessage)
		}
		text := modelText("steer", *guidance.Message)
		if len(guidance.JobReadGrants) != 0 {
			notice, err := NewJobReadGrantNotice(guidance.JobReadGrants).ModelContent()
			if err != nil {
				return "", err
			}
			text += "\n\n" + notice
		}
		return text, nil
	case TypeRuntimeContext:
		fields, err := modelObject(m.Content)
		if err != nil {
			return "", err
		}
		var kind string
		if err := json.Unmarshal(fields["type"], &kind); err != nil {
			return "", fmt.Errorf(prompts.LLMInvalidContextKind)
		}
		switch kind {
		case "runtime_context":
			delete(fields, "type")
		case "runtime_context_update":
			fields, err = modelObject(string(fields["set"]))
			if err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf(prompts.LLMInvalidContextKind)
		}
		return projectRuntimeContext(kind, fields)
	case TypeRuntimeNotification:
		fields, err := modelObject(m.Content)
		if err != nil {
			// Some notifications contain plain external text rather than an
			// event object. It remains data inside a structured notification.
			return FormatModelPayload(ModelPayload{Metadata: PayloadMetadata{Kind: "runtime_notification", Field: "event"}, Text: m.Content})
		}
		delete(fields, "message_type")
		var outputOmitted bool
		_ = json.Unmarshal(fields["output_omitted"], &outputOmitted)
		// Omitted output can be recovered from its canonical source event;
		// ordinary notices need no delivery bookkeeping in model context.
		if !outputOmitted {
			delete(fields, "event_seq")
		}
		return projectNotification(fields)
	case TypeCompactionContext:
		return modelText("compaction_context", m.Content), nil
	case TypeRuntimeInstruction:
		return modelText("runtime_instruction", m.Content), nil
	}
	return m.Content, nil
}

// NewJobReadGrantNotice records read-access metadata separately from task prose.
// Serialization copies the IDs; modifying the caller's slice cannot change the
// notice. Delivery and backend access changes remain the session's responsibility.
func NewJobReadGrantNotice(ids []string) Message {
	// This fixed record contains only strings and slices, which cannot fail
	// JSON encoding. No caller-owned mutable values survive serialization.
	data, _ := json.Marshal(struct {
		Type   string   `json:"type"`
		JobIDs []string `json:"job_ids"`
	}{Type: "job_read_grants", JobIDs: ids})
	return Message{Type: TypeRuntimeNotification, Content: string(data)}
}

func modelText(name, text string) string {
	return "<" + name + ">\n" + text + "\n</" + name + ">"
}

func modelObject(content string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &fields); err != nil {
		return nil, fmt.Errorf(prompts.LLMInvalidModelObject, err)
	}
	if fields == nil {
		return nil, fmt.Errorf(prompts.LLMModelObjectRequired)
	}
	return fields, nil
}

// modelObjectData decodes JSON with its own escape rules and preserves lexical
// numbers. Parsing JSON as YAML would reject escaped UTF-16 surrogate pairs and
// can mistake numbers outside built-in numeric ranges for strings.
func modelObjectData(name string, fields map[string]json.RawMessage) (string, error) {
	data, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", fmt.Errorf(prompts.LLMInvalidModelObject, err)
	}
	return FormatModelData(name, modelNumbers(value))
}

// modelNumber supplies an explicit YAML numeric tag without converting through
// float64 or limiting JSON integers to the host's integer range.
type modelNumber string

// MarshalYAML retains the original number token with its JSON numeric kind.
func (n modelNumber) MarshalYAML() (any, error) {
	tag := "!!int"
	if strings.ContainsAny(string(n), ".eE") {
		tag = "!!float"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: string(n)}, nil
}

// modelNumbers operates only on the locally owned JSON decoder result. Other
// scalars keep their concrete types, so YAML's normal string escaping applies.
func modelNumbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		return modelNumber(v)
	case string:
		// YAML resolvers disagree on numeric overflow. Quote JSON-number
		// spellings explicitly when the original value is a string.
		if len(v) != 0 && (v[0] == '-' || v[0] >= '0' && v[0] <= '9') && json.Valid([]byte(v)) {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.DoubleQuotedStyle, Value: v}
		}
	case map[string]any:
		for key, child := range v {
			v[key] = modelNumbers(child)
		}
	case []any:
		for i, child := range v {
			v[i] = modelNumbers(child)
		}
	}
	return value
}

// FormatModelData serializes structured model-facing data as two-space-indented
// YAML inside one XML-style marker. Callers supply fixed schema names, never
// names inferred from authored content. Serialization preserves scalar values
// and multiline text; callers retain ownership of value.
func FormatModelData(name string, value any) (string, error) {
	if name == "" || strings.IndexFunc(name, func(r rune) bool { return r != '_' && (r < 'a' || r > 'z') }) >= 0 {
		return "", fmt.Errorf(prompts.LLMInvalidModelMarker, name)
	}
	var body strings.Builder
	body.WriteString("<" + name + ">\n")
	encoder := yaml.NewEncoder(&body)
	encoder.SetIndent(2)
	if err := encoder.Encode(value); err != nil {
		return "", fmt.Errorf(prompts.LLMModelYAML, err)
	}
	if err := encoder.Close(); err != nil {
		return "", fmt.Errorf(prompts.LLMModelYAML, err)
	}
	body.WriteString("</" + name + ">")
	return body.String(), nil
}
