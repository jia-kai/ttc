package context

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestRetentionRecognizesPriorCompactionContextWithoutNewAssistantWork(t *testing.T) {
	messages := []llm.Message{
		llm.NewCompactionContext("Earlier work"),
		{Type: llm.TypeUserInput, Content: "Continue the task", InputTimeMS: 500},
	}
	for iteration := range 3 {
		retention, err := Retain(messages, 0, 1000)
		if err != nil || retention.Start != len(messages) || !reflect.DeepEqual(retention.Inputs, []int{1}) {
			t.Fatalf("iteration %d: prior context was not older work: %+v, %v", iteration, retention, err)
		}
		inputs, err := RetainedInputMessages(messages, retention.Inputs, time.UnixMilli(int64(1000+iteration)))
		if err != nil {
			t.Fatal(err)
		}
		messages = append([]llm.Message{llm.NewCompactionContext("Earlier work summarized again")}, inputs...)
		if messages[1].InputTimeMS != 500 {
			t.Fatal("repeated compaction refreshed original human input age")
		}
	}
}

func TestRetentionCompactionContextIsNotAnAssistantCycle(t *testing.T) {
	messages := []llm.Message{
		llm.NewCompactionContext("Older summarized work"),
		{Type: llm.TypeUserInput, Content: "Continue"},
		{Type: llm.TypeAssistant, Content: "First recent response"},
		llm.NewCompactionContext("Reference data between assistant responses"),
		{Type: llm.TypeAssistant, Content: "Second recent response"},
		llm.NewCompactionContext("Trailing reference data"),
	}
	retention, err := Retain(messages, 0, 10000)
	if err != nil || retention.Start != 2 || !reflect.DeepEqual(retention.Inputs, []int{1}) {
		t.Fatal("context counted as an assistant cycle or cycle boundary", retention, err)
	}
}

func TestRetentionCompactionContextDoesNotConsumeFreshResults(t *testing.T) {
	for _, result := range []llm.Message{
		{Type: llm.TypeToolResult, CallID: "read", Content: strings.Repeat("fresh result ", 1000)},
		{Type: llm.TypeToolResult, CallID: "read", Files: []llm.BinaryFile{{Path: "/document.pdf", MIMEType: "application/pdf", Bytes: 20 << 10}}},
	} {
		messages := []llm.Message{
			llm.NewCompactionContext("Older summarized work"),
			{Type: llm.TypeUserInput, Content: "Read"},
			{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "read"}}},
			result,
			llm.NewCompactionContext("Generated context is not a coding-model response"),
		}
		retention, err := Retain(messages, 0, 100)
		if err != nil || retention.Start != 2 || !reflect.DeepEqual(retention.Inputs, []int{1}) {
			t.Fatal("context consumed fresh tool results", retention, err)
		}
		if _, err := Retain(messages[1:], 0, 100); !errors.Is(err, ErrNoCompactableHistory) {
			t.Fatal("context after fresh results manufactured older work", err)
		}
	}
}

func TestRetentionRejectsMetadataOnlyOlderHistory(t *testing.T) {
	for _, typ := range []llm.MessageType{llm.TypeSystemInstruction, llm.TypeDeveloperInstruction, llm.TypeRuntimeContext, llm.TypeRuntimeNotification, llm.TypeRuntimeInstruction, llm.TypeChildGuidance} {
		messages := []llm.Message{
			{Type: typ, Content: "Metadata is not summarized work"},
			{Type: llm.TypeUserInput, Content: "Continue"},
		}
		if _, err := Retain(messages, 0, 1000); !errors.Is(err, ErrNoCompactableHistory) {
			t.Fatalf("%s metadata alone made compaction progress: %v", typ, err)
		}
	}
}
