package context

import (
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestRetentionDiagnosticBytes(t *testing.T) {
	for _, test := range []struct {
		messages []llm.Message
		want     string
	}{
		{nil, "nothing to compact"},
		{[]llm.Message{{Role: "tool", CallID: "missing"}}, "unpaired tool result prevents compaction"},
		{[]llm.Message{{Role: "user", InputSource: "invalid"}}, `human input 0: invalid human input source "invalid"`},
	} {
		_, err := Retain(test.messages, 1, 100)
		if err == nil || err.Error() != test.want {
			t.Fatalf("retention error = %v; want %q", err, test.want)
		}
	}
	_, err := RetainedInputMessages([]llm.Message{{Role: "user", InputTimeMS: 2000}}, []int{0}, time.UnixMilli(1000))
	if want := "retained input 0: retained input commit timestamp 2000 is after compaction timestamp 1000"; err == nil || err.Error() != want {
		t.Fatalf("retained input error = %v; want %q", err, want)
	}
}
