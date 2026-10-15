package session

import (
	"testing"

	"ttc/internal/llm"
)

// seedIsolatedHistory gives direct compactChild fixtures the durable actor input
// that normal runtime admission commits before compaction. Call once per original
// input; retries and projected summaries must not fabricate duplicate evidence.
func seedIsolatedHistory(t *testing.T, r *Runtime, actor, turn string, messages []llm.Message) {
	t.Helper()
	for _, message := range messages {
		if _, err := r.Store.Append(r.Current(), turn, actor, "message", message.Role(), false, message); err != nil {
			t.Fatal(err)
		}
	}
}
