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
	var group struct {
		Inputs []sessionRetainedEntry `yaml:"inputs"`
	}
	decodeSessionModelData(t, content, "retained_inputs", &group)
	return group.Inputs
}
