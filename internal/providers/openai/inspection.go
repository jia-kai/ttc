package openai

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"ttc/internal/llm"
)

// maxInspectionDepth bounds both native nesting and recursively embedded JSON.
// Deeper values remain available as opaque JSON and in the original raw capture.
const maxInspectionDepth = 64

var _ llm.InspectionParser = (*Adapter)(nil)

// InspectBoundary interprets OpenAI native bodies/items without modifying their
// bytes or consulting transport state. Unknown fields remain visible.
func (*Adapter) InspectBoundary(direction string, payload json.RawMessage) (llm.InspectionDocument, error) {
	value, err := decodeInspectionJSON(payload)
	if err != nil {
		return llm.InspectionDocument{}, fmt.Errorf("inspect OpenAI %s: %w", direction, err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return llm.InspectionDocument{}, fmt.Errorf("inspect OpenAI %s: expected object", direction)
	}
	var sections []llm.InspectionSection
	var items []llm.InspectionItem
	state := ""
	switch direction {
	case "request":
		items, err = inspectRequest(object)
		if err != nil {
			return llm.InspectionDocument{}, fmt.Errorf("inspect OpenAI request items: %w", err)
		}
	case "response":
		sections = append(sections, llm.InspectionSection{Title: "Response", Fields: inspectionFields(object, 0)})
	case "outcome":
		sections = append(sections, llm.InspectionSection{Title: "Outcome", Fields: inspectionFields(object, 0)})
		switch object["status"] {
		case "success":
			state = "completed"
		case "failure":
			state = "failed"
			if object["category"] == "canceled" {
				state = "interrupted"
			}
		}
	default:
		return llm.InspectionDocument{}, fmt.Errorf("unsupported OpenAI inspection direction %q", direction)
	}
	return llm.InspectionDocument{Sections: sections, Items: items, State: state}, nil
}

func inspectRequest(object map[string]any) ([]llm.InspectionItem, error) {
	keys := make([]string, 0, len(object))
	for name := range object {
		if name != "instructions" && name != "tools" && name != "input" {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	items := make([]llm.InspectionItem, 0, len(keys))
	add := func(section, name string, native any, value llm.InspectionValue) error {
		raw, err := json.Marshal(native)
		if err != nil {
			return err
		}
		hash := sha256.New()
		hash.Write([]byte(section))
		hash.Write([]byte{0})
		hash.Write(raw)
		items = append(items, llm.InspectionItem{Section: section, Name: name,
			Hash: fmt.Sprintf("%x", hash.Sum(nil)), JSON: raw, Value: value})
		return nil
	}
	for _, key := range keys {
		if err := add("Settings", key, map[string]any{key: object[key]}, inspectionValue(key, object[key], 1)); err != nil {
			return nil, err
		}
	}
	for _, part := range []struct{ key, title string }{
		{"instructions", "Instructions"}, {"tools", "Tool definitions"}, {"input", "Input messages"},
	} {
		native, exists := object[part.key]
		if !exists {
			continue
		}
		values, array := native.([]any)
		if array && part.key != "instructions" && len(values) > 0 {
			for i, nativeItem := range values {
				name := inspectionItemName(nativeItem, i)
				if part.key == "tools" {
					if tool, ok := nativeItem.(map[string]any); ok {
						if toolName, ok := tool["name"].(string); ok && toolName != "" {
							name = toolName
						}
					}
				}
				if err := add(part.title, name, nativeItem, inspectionValue("", nativeItem, 0)); err != nil {
					return nil, err
				}
			}
			continue
		}
		value := inspectionValue(part.key, native, 0)
		name := part.key
		if part.key == "instructions" || array {
			// Empty collections carry their explicit native field presence.
			// The wrapper distinguishes them from ordinary array elements.
			native = map[string]any{part.key: native}
		} else if part.key == "input" {
			name = inspectionItemName(native, 0)
		}
		if err := add(part.title, name, native, value); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func decodeInspectionJSON(payload []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value")
		}
		return nil, fmt.Errorf("trailing JSON: %w", err)
	}
	return value, nil
}

func inspectionItemName(item any, index int) string {
	name := fmt.Sprintf("%d", index+1)
	if object, ok := item.(map[string]any); ok {
		for _, key := range []string{"type", "role", "name"} {
			if text, ok := object[key].(string); ok && text != "" {
				name += " · " + text
			}
		}
	}
	return name
}

func inspectionFields(object map[string]any, depth int) []llm.InspectionField {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]llm.InspectionField, 0, len(keys))
	for _, key := range keys {
		fields = append(fields, llm.InspectionField{Name: key, Value: inspectionValue(key, object[key], depth+1)})
	}
	return fields
}

func inspectionValue(name string, value any, depth int) llm.InspectionValue {
	if depth >= maxInspectionDepth {
		if text, ok := value.(string); ok {
			return llm.InspectionValue{Kind: llm.InspectionKindOpaque, Text: text}
		}
		raw, _ := json.Marshal(value) // Values originate from the JSON decoder.
		return llm.InspectionValue{Kind: llm.InspectionKindOpaque, Text: string(raw)}
	}
	switch value := value.(type) {
	case map[string]any:
		return llm.InspectionValue{Kind: llm.InspectionKindObject, Fields: inspectionFields(value, depth)}
	case []any:
		items := make([]llm.InspectionValue, 0, len(value))
		for _, item := range value {
			items = append(items, inspectionValue(name, item, depth+1))
		}
		return llm.InspectionValue{Kind: llm.InspectionKindArray, Items: items}
	case string:
		if name == "file_data" || strings.Contains(strings.ToLower(name), "encrypted") || strings.HasPrefix(strings.TrimSpace(value), "data:") {
			return llm.InspectionValue{Kind: llm.InspectionKindOpaque, Text: value}
		}
		if name == "text" || name == "arguments" || name == "output" || name == "content" {
			text := strings.TrimSpace(value)
			if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
				if embedded, err := decodeInspectionJSON([]byte(text)); err == nil {
					return inspectionValue("", embedded, depth+1)
				}
			}
		}
		return llm.InspectionValue{Kind: llm.InspectionKindString, Text: value}
	case json.Number:
		return llm.InspectionValue{Kind: llm.InspectionKindNumber, Text: string(value)}
	case bool:
		return llm.InspectionValue{Kind: llm.InspectionKindBool, Text: fmt.Sprint(value)}
	default: // JSON null; all other decoder-produced types are handled above.
		return llm.InspectionValue{Kind: llm.InspectionKindNull}
	}
}
