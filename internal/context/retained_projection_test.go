package context

import (
	"reflect"
	"testing"
	"ttc/internal/llm"
)

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
		{Type: llm.TypeUserInput, Content: "first tie", InputTimeMS: 1000, RetainedAtMS: 9000},
		{Type: llm.TypeUserInput, Content: "second tie", InputSource: "steer", InputTimeMS: 1000, RetainedAtMS: 10000},
		{Type: llm.TypeAssistant, Content: "separator"},
		{Type: llm.TypeUserInput, Content: "separate", InputTimeMS: 500, RetainedAtMS: 9000},
	}
	got, err := ProjectRetainedInputs(messages)
	if err != nil || len(got) != 5 || got[2].Content != "separator" {
		t.Fatal(got, err)
	}
	want := `{"type":"retained_inputs","inputs":[{"type":"user_input","source":"normal","timestamp":"8.00 seconds before compaction","content":"first tie"},{"type":"user_steer","source":"steer","timestamp":"9.00 seconds before compaction","content":"second tie"}]}`
	if got[1].Content != want {
		t.Fatal(got[1].Content)
	}
}
