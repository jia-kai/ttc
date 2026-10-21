package session

import (
	"context"
	"reflect"
	"testing"

	contextbuild "ttc/internal/context"
)

func TestSteerInputsPreservesUnitsAttachmentsAndAtomicCancellation(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.mu.Lock()
	r.activeTurn = "active-fixture"
	_, r.activeCancel = context.WithCancel(context.Background())
	r.mu.Unlock()
	inputs := []contextbuild.Input{
		{Text: "first raw instruction", Source: "queue", Attachments: []contextbuild.Attachment{{Path: "first.txt", Kind: "file", Text: "first evidence"}}},
		{Text: "second raw instruction", Source: "queue", Attachments: []contextbuild.Attachment{{Path: "second.txt", Kind: "file", Text: "second evidence"}}},
	}
	if err := r.SteerInputs(inputs); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.steers, inputs) {
		t.Fatal("batch flattened or changed original inputs", r.steers)
	}
	// A failed batch must not partially accept an earlier valid member.
	if err := r.SteerInputs([]contextbuild.Input{{Text: "valid"}, {Text: ""}}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if !reflect.DeepEqual(r.steers, inputs) {
		t.Fatal("failed batch partially queued input", r.steers)
	}
	cancelled, err := r.CancelSteer()
	if err != nil || !reflect.DeepEqual(cancelled, inputs[1]) {
		t.Fatal("cancellation changed the last input unit", cancelled, err)
	}
	if !reflect.DeepEqual(r.steers, inputs[:1]) {
		t.Fatal("cancellation changed earlier input", r.steers)
	}
}
