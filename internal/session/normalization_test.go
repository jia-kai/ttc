package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQuestionEmptyOptionsAreFreeText(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	codec, _ := r.Tools.Get("question")
	for _, options := range []string{`[]`, `null`} {
		call, err := codec.DecodeCall(1, []byte(`{"questions":[{"id":" q ","prompt":" Q ","options":`+options+`}]}`))
		if err != nil {
			t.Fatal(options, err)
		}
		_, encoded, _ := call.Encode()
		if !strings.Contains(string(encoded), `"id":" q "`) {
			t.Fatal("trimmed identity", string(encoded))
		}
	}
	for _, input := range []string{
		`{"questions":[{"id":"q","prompt":"Q","options":[],"recommended_option_id":"a"}]}`,
		`{"questions":[{"id":"q","prompt":"Q","options":[],"multiple":false}]}`,
		`{"questions":[{"id":"q","prompt":"Q","options":[{"id":"a","label":"A"}]}]}`,
	} {
		if _, err := codec.DecodeCall(1, []byte(input)); err == nil {
			t.Fatal("accepted", input)
		}
	}
}

func TestConversationExportEnumNormalization(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	codec, _ := r.Tools.Get("conversation_export")
	input := []byte(`{"path":" /path with spaces ","format":" JSONL ","scope":" BEFORE_COMPACTION "}`)
	call, err := codec.DecodeCall(1, input)
	if err != nil {
		t.Fatal(err)
	}
	_, encoded, _ := call.Encode()
	var args conversationExportArgs
	if err := json.Unmarshal(encoded, &args); err != nil || args.Format != "jsonl" || args.Scope != "before_compaction" || args.Path != " /path with spaces " {
		t.Fatal(string(encoded), err)
	}
	if len(call.InputWarnings()) != 2 {
		t.Fatal(call.InputWarnings())
	}
	warnings, err := json.Marshal(call.InputWarnings())
	if err != nil || strings.Contains(string(warnings), `"minimum"`) || strings.Contains(string(warnings), `"maximum"`) {
		t.Fatal(string(warnings), err)
	}
	for _, field := range []string{`"format":"md"`, `"scope":"before compaction"`, `"unknown":false`} {
		if _, err := codec.DecodeCall(1, []byte(`{"path":"x",`+field+`}`)); err == nil {
			t.Fatal("accepted", field)
		}
	}
}
