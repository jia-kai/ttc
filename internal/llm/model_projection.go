package llm

import (
	"encoding/json"
	"strings"
)

// modelPayloadReference is local to one projected message; indices are zero based
// in the order raw blocks follow its structured metadata.
type modelPayloadReference struct {
	PayloadIndex int `json:"payload_index"`
}

func rawString(fields map[string]json.RawMessage, key string) string {
	var text string
	_ = json.Unmarshal(fields[key], &text)
	return text
}

func projectNotification(fields map[string]json.RawMessage) (string, error) {
	var payloads []ModelPayload
	for _, field := range []string{"answer", "stdout", "stderr", "message", "error", "warning", "transcript_export_error"} {
		raw, ok := fields[field]
		if !ok {
			continue
		}
		var text string
		// Non-string fields, including structured errors and null, remain metadata.
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &text) != nil {
			continue
		}
		// Empty streams carry no output; keep their explicit empty scalars in
		// event metadata rather than spend framing on empty job_output blocks.
		if text == "" && (field == "stdout" || field == "stderr") {
			continue
		}
		metadata := PayloadMetadata{Kind: "runtime_notification", Type: rawString(fields, "type"), Field: field,
			ActorID: rawString(fields, "actor_id"), ChildID: rawString(fields, "child_id"), JobID: rawString(fields, "job_id")}
		if field == "stdout" || field == "stderr" {
			metadata.Kind = "job_output"
			metadata.Stream = field
		}
		_ = json.Unmarshal(fields[field+"_delivery_truncated"], &metadata.DeliveryTruncated)
		_ = json.Unmarshal(fields[field+"_truncated"], &metadata.Truncated)
		fields[field], _ = json.Marshal(modelPayloadReference{len(payloads)})
		payloads = append(payloads, ModelPayload{Metadata: metadata, Text: text})
	}
	return modelDataPayloads("runtime_notification", fields, payloads)
}

func projectRuntimeContext(kind string, fields map[string]json.RawMessage) (string, error) {
	var payloads []ModelPayload
	// Decode only the known path. Keeping other values as RawMessage preserves
	// unknown fields, explicit nulls, empty collections and number lexemes.
	if raw, ok := fields["project"]; ok && string(raw) != "null" {
		project, err := modelObject(string(raw))
		if err != nil {
			return "", err
		}
		if raw, ok := project["instructions"]; ok && string(raw) != "null" {
			var instructions []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &instructions); err != nil {
				return "", err
			}
			for index, instruction := range instructions {
				raw, ok := instruction["content"]
				if !ok {
					continue
				}
				var text string
				if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &text) != nil {
					continue
				}
				i := index
				payloads = append(payloads, ModelPayload{Metadata: PayloadMetadata{Kind: "project_instruction", Scope: kind, Path: rawString(instruction, "path"), Index: &i}, Text: text})
				instruction["content"], _ = json.Marshal(modelPayloadReference{len(payloads) - 1})
			}
			project["instructions"], _ = json.Marshal(instructions)
		}
		fields["project"], _ = json.Marshal(project)
	}
	return modelDataPayloads(kind, fields, payloads)
}

func modelDataPayloads(name string, fields map[string]json.RawMessage, payloads []ModelPayload) (string, error) {
	metadata, err := modelObjectData(name, fields)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString(metadata)
	for _, payload := range payloads {
		text, err := FormatModelPayload(payload)
		if err != nil {
			return "", err
		}
		out.WriteString("\n\n")
		out.WriteString(text)
	}
	return out.String(), nil
}
