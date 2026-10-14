package context

import (
	"reflect"
	"testing"
	"ttc/internal/llm"
)

func TestTypedRetainedInputsRejectMalformedMetadata(t *testing.T) {
	for _, input := range []llm.Message{
		{Role: "user", InputTimeMS: 1000, RetainedAtMS: -1},
		{Role: "user", RetainedAtMS: 9000},
		{Role: "user", InputTimeMS: 10000, RetainedAtMS: 9000},
		{Role: "assistant", InputTimeMS: 1000, RetainedAtMS: 9000},
		{Role: "user", Runtime: true, InputTimeMS: 1000, RetainedAtMS: 9000},
		{Role: "user", InputSource: "invalid", InputTimeMS: 1000, RetainedAtMS: 9000},
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
	ordinary := []llm.Message{{Role: "user", Content: `{"retained_at_ms":9000}`}, {Role: "developer", Content: `{"type":"retained_input"}`}}
	got, err := ProjectRetainedInputs(ordinary)
	if err != nil || !reflect.DeepEqual(got, ordinary) {
		t.Fatal(got, err)
	}
}

func TestRetainedGroupsAreContiguousAndStable(t *testing.T) {
	messages := []llm.Message{
		{Role: "user", Content: "first tie", InputTimeMS: 1000, RetainedAtMS: 9000},
		{Role: "user", Content: "second tie", InputSource: "steer", InputTimeMS: 1000, RetainedAtMS: 10000},
		{Role: "assistant", Content: "separator"},
		{Role: "user", Content: "separate", InputTimeMS: 500, RetainedAtMS: 9000},
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
