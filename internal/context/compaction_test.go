package context

import (
	"reflect"
	"testing"

	"ttc/internal/llm"
)

func TestCompactionReplacementPreservesCanonicalMetadataWithoutReplayState(t *testing.T) {
	state := &llm.ReplayState{Provider: "test", Model: "fixture"}
	handoff := llm.NewCompactionContext("checked summary")
	retained := []llm.Message{{Type: llm.TypeUserInput, Content: "original instruction", InputSource: "steer", InputTimeMS: 100, RetainedAtMS: 200, State: state}}
	tail := []llm.Message{{Type: llm.TypeAssistant, Content: "recent response", RequestID: 7, State: state}}
	pending := &llm.Message{Type: llm.TypeRuntimeInstruction, Content: "pending recovery", RequestID: 9}
	got := CompactionReplacement(handoff, retained, tail, pending)
	wantInput, wantTail := retained[0], tail[0]
	wantInput.State, wantTail.State = nil, nil
	want := []llm.Message{handoff, wantInput, wantTail, *pending}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("incorrect canonical replacement", got, want)
	}
	if retained[0].State != state || tail[0].State != state {
		t.Fatal("materialization changed source messages")
	}
	got[1].Content = "modified output"
	if retained[0].Content != "original instruction" {
		t.Fatal("replacement aliases caller slice")
	}
	alreadyPresent := append(tail, *pending)
	got = CompactionReplacement(handoff, retained, alreadyPresent, pending)
	if len(got) != 4 {
		t.Fatal("duplicated pending recovery instruction", got)
	}
}
