package session

import (
	"io"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
	"ttc/internal/llm"
)

func decodeSessionModelData(t *testing.T, content, name string, value any) {
	t.Helper()
	start, end := "<"+name+">\n", "\n</"+name+">"
	if !strings.HasPrefix(content, start) || !strings.HasSuffix(content, end) {
		t.Fatalf("missing %s wrapper: %q", name, content)
	}
	decoder := yaml.NewDecoder(strings.NewReader(strings.TrimSuffix(strings.TrimPrefix(content, start), end)))
	decoder.KnownFields(true)
	if err := decoder.Decode(value); err != nil {
		t.Fatalf("invalid %s YAML: %v\n%s", name, err, content)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("unexpected trailing %s data: %v", name, err)
	}
}

type sessionRetainedEntry struct {
	Type      string `yaml:"type"`
	Timestamp string `yaml:"timestamp"`
	Content   string `yaml:"content"`
	Indices   []int  `yaml:"attachment_indices,omitempty"`
}

func sessionRetainedInputs(t *testing.T, message llm.Message) []sessionRetainedEntry {
	t.Helper()
	if message.Type != llm.TypeUserInput || message.Role() != "user" || message.InputSource != "" || message.RetainedAtMS != 0 {
		t.Fatalf("retained group is not a projected user message: %+v", message)
	}
	content, err := message.ModelContent()
	if err != nil || content != message.Content {
		t.Fatalf("retained group was wrapped twice: %q, %v", content, err)
	}
	const start, end = "<retained_inputs>\n", "\n</retained_inputs>"
	if !strings.HasPrefix(content, start) || !strings.HasSuffix(content, end) {
		t.Fatalf("missing retained_inputs wrapper: %q", content)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(content, start), end)
	var entries []sessionRetainedEntry
	for body != "" {
		header, rest, ok := strings.Cut(body, ":\n")
		if !ok || !strings.HasPrefix(header, "<<<") {
			t.Fatalf("invalid retained payload header: %q", body)
		}
		id := strings.TrimPrefix(header, "<<<")
		metadata, rest, ok := strings.Cut(rest, ">>>\n")
		if !ok {
			t.Fatalf("missing retained payload metadata terminator: %q", body)
		}
		var fields llm.PayloadMetadata
		decoder := yaml.NewDecoder(strings.NewReader(metadata))
		decoder.KnownFields(true)
		if err := decoder.Decode(&fields); err != nil {
			t.Fatalf("invalid retained payload metadata: %v\n%s", err, metadata)
		}
		if fields.Kind != "retained_input" {
			t.Fatalf("wrong retained payload kind: %+v", fields)
		}
		text, tail, ok := strings.Cut(rest, "\n<<</"+id+">>>")
		if !ok {
			t.Fatalf("missing retained payload closing marker: %q", body)
		}
		entry := sessionRetainedEntry{Type: fields.Type, Timestamp: fields.Timestamp, Content: text}
		if fields.AttachmentIndices != nil {
			entry.Indices = *fields.AttachmentIndices
		}
		entries = append(entries, entry)
		if tail != "" && !strings.HasPrefix(tail, "\n") {
			t.Fatalf("invalid retained payload separator: %q", tail)
		}
		body = strings.TrimPrefix(tail, "\n")
	}
	return entries
}
