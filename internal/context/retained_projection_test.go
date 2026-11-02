package context

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"ttc/internal/llm"

	"go.yaml.in/yaml/v3"
)

type retainedProjectionEntry struct {
	Type              string `yaml:"type"`
	Timestamp         string `yaml:"timestamp"`
	Content           string `yaml:"content"`
	AttachmentIndices []int  `yaml:"attachment_indices,omitempty"`
}

func retainedProjectionEntries(t *testing.T, message llm.Message) []retainedProjectionEntry {
	t.Helper()
	const start, end = "<retained_inputs>\n", "\n</retained_inputs>"
	if message.Type != llm.TypeUserInput || message.Role() != "user" || !strings.HasPrefix(message.Content, start) || !strings.HasSuffix(message.Content, end) {
		t.Fatalf("not a single tagged user message: %+v", message)
	}
	content, err := message.ModelContent()
	if err != nil || content != message.Content {
		t.Fatalf("retained input was wrapped twice: %q, %v", content, err)
	}
	var group struct {
		Inputs []retainedProjectionEntry `yaml:"inputs"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(strings.TrimSuffix(strings.TrimPrefix(content, start), end)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&group); err != nil {
		t.Fatalf("invalid retained YAML: %v\n%s", err, content)
	}
	return group.Inputs
}

func TestTypedRetainedInputsRejectMalformedMetadata(t *testing.T) {
	for _, input := range []llm.Message{
		{Type: llm.TypeUserInput, InputTimeMS: 1000, RetainedAtMS: -1},
		{Type: llm.TypeUserInput, RetainedAtMS: 9000},
		{Type: llm.TypeUserInput, InputTimeMS: 10000, RetainedAtMS: 9000},
		{Type: llm.TypeAssistant, InputTimeMS: 1000, RetainedAtMS: 9000},
		{Type: llm.TypeRuntimeNotification, InputTimeMS: 1000, RetainedAtMS: 9000},
		{Type: llm.TypeUserInput, InputSource: "invalid", InputTimeMS: 1000, RetainedAtMS: 9000},
	} {
		messages := []llm.Message{input}
		if err := ValidateRetainedInputs(messages); err == nil {
			t.Fatal("accepted malformed metadata", input)
		}
		if _, err := ProjectRetainedInputs(messages); err == nil {
			t.Fatal("projected malformed metadata", input)
		}
		if Tokens(messages) != -1 || Fits(llm.Selection{Model: llm.ScriptModel()}, "", nil, messages, false) {
			t.Fatal("malformed metadata passed admission", input)
		}
	}
	ordinary := []llm.Message{{Type: llm.TypeUserInput, Content: `{"retained_at_ms":9000}`}, {Type: llm.TypeDeveloperInstruction, Content: `{"type":"retained_input"}`}}
	got, err := ProjectRetainedInputs(ordinary)
	if err != nil || !reflect.DeepEqual(got, ordinary) {
		t.Fatal(got, err)
	}
}

func TestRetainedGroupsAreContiguousAndStable(t *testing.T) {
	messages := []llm.Message{
		{Type: llm.TypeUserInput, Content: "ordinary prefix"},
		{Type: llm.TypeUserInput, Content: "first tie", InputTimeMS: 1000, RetainedAtMS: 9000},
		{Type: llm.TypeUserInput, Content: "second tie", InputSource: "steer", InputTimeMS: 1000, RetainedAtMS: 10000},
		{Type: llm.TypeUserInput, Content: "earlier", InputSource: "queue", InputTimeMS: 999, RetainedAtMS: 9000},
		{Type: llm.TypeAssistant, Content: "separator"},
		{Type: llm.TypeUserInput, Content: "separate", InputTimeMS: 500, RetainedAtMS: 9000},
	}
	got, err := ProjectRetainedInputs(messages)
	if err != nil || len(got) != 4 || !reflect.DeepEqual(got[0], messages[0]) || got[2].Content != "separator" {
		t.Fatal(got, err)
	}
	want := []retainedProjectionEntry{
		{Type: "queued_user_input", Timestamp: "8.00 seconds before compaction", Content: "earlier"},
		{Type: "user_input", Timestamp: "8.00 seconds before compaction", Content: "first tie"},
		{Type: "user_steer", Timestamp: "9.00 seconds before compaction", Content: "second tie"},
	}
	if entries := retainedProjectionEntries(t, got[1]); !reflect.DeepEqual(entries, want) {
		t.Fatal(entries)
	}
	if entries := retainedProjectionEntries(t, got[3]); !reflect.DeepEqual(entries, []retainedProjectionEntry{{Type: "user_input", Timestamp: "8.50 seconds before compaction", Content: "separate"}}) {
		t.Fatal(entries)
	}
}

func TestRetainedProjectionPreservesTextAndNativeOccurrences(t *testing.T) {
	file := llm.BinaryFile{Path: "/snapshot/image.png", SHA256: strings.Repeat("a", 64), MIMEType: "image/png", Bytes: 8}
	text := "\ntrue\nnull\n123\nkey: value\n---\n<retained_inputs>\n</retained_inputs>\n\tquoted: \"yes\"\n\n"
	messages := []llm.Message{
		{Type: llm.TypeUserInput, Content: text, InputSource: "task", InputTimeMS: 1234, RetainedAtMS: 9999, Files: []llm.BinaryFile{file, file}},
		{Type: llm.TypeUserInput, Content: "", InputSource: "btw", InputTimeMS: 1235, RetainedAtMS: 9999, Files: []llm.BinaryFile{file}},
		{Type: llm.TypeUserInput, Content: "true", InputSource: "normal", InputTimeMS: 1236, RetainedAtMS: 9999},
	}
	// Canonical order and exact-time order can differ. Files and their indices
	// must follow the projected inputs, including identical native occurrences.
	messages[0], messages[1] = messages[1], messages[0]
	before, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ProjectRetainedInputs(messages)
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	want := []retainedProjectionEntry{
		{Type: "child_task", Timestamp: "8.77 seconds before compaction", Content: text, AttachmentIndices: []int{0, 1}},
		{Type: "aside_task", Timestamp: "8.76 seconds before compaction", Content: "", AttachmentIndices: []int{2}},
		{Type: "user_input", Timestamp: "8.76 seconds before compaction", Content: "true"},
	}
	if entries := retainedProjectionEntries(t, got[0]); !reflect.DeepEqual(entries, want) || !reflect.DeepEqual(got[0].Files, []llm.BinaryFile{file, file, file}) {
		t.Fatal("lost text, input type, stable time or native occurrence", entries, got[0].Files)
	}
	again, err := ProjectRetainedInputs(got)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatal("projection is not idempotent", again, err)
	}
	after, err := json.Marshal(messages)
	if err != nil || string(before) != string(after) {
		t.Fatal("projection mutated canonical messages", err)
	}
}

func TestRetainedProjectionTokenAccountingUsesSingleWrapper(t *testing.T) {
	messages := []llm.Message{
		llm.NewCompactionContext("raw summary\nwith details"),
		{Type: llm.TypeUserInput, Content: "expanded original text", InputTimeMS: 1000, RetainedAtMS: 9000,
			Files: []llm.BinaryFile{{MIMEType: "image/png"}, {MIMEType: "image/png"}}},
		{Type: llm.TypeUserInput, Content: "keep precision", InputSource: "steer", InputTimeMS: 1001, RetainedAtMS: 9000},
	}
	projected, err := ProjectRetainedInputs(messages)
	if err != nil || len(projected) != 2 {
		t.Fatal(projected, err)
	}
	retainedProjectionEntries(t, projected[1])
	summary, err := projected[0].ModelContent()
	if err != nil {
		t.Fatal(err)
	}
	want := 16 + Estimate(summary) + 16 + Estimate(projected[1].Content)
	for _, file := range projected[1].Files {
		want += file.EstimatedTokens()
	}
	if canonical, grouped := Tokens(messages), Tokens(projected); canonical != want || grouped != want {
		t.Fatalf("single-wrapper tokens: canonical=%d grouped=%d want=%d", canonical, grouped, want)
	}
	snapshot := estimatorSnapshot()
	snapshot.Messages = messages
	canonical := EstimateInput(snapshot)
	snapshot.Messages = projected
	grouped := EstimateInput(snapshot)
	if want := want + Estimate(snapshot.System); canonical.Input != want || grouped.Input != want {
		t.Fatalf("single-wrapper estimation: canonical=%+v grouped=%+v want=%d", canonical, grouped, want)
	}
	var estimator Estimator
	if prepared := estimator.Prepare(snapshot); prepared.Input != grouped.Input || prepared.LocalInput != grouped.LocalInput {
		t.Fatal("prepared admission disagrees with stateless estimate", prepared, grouped)
	}
}
