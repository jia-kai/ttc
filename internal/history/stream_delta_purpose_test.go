package history

import (
	"encoding/json"
	"testing"

	"ttc/internal/llm"
)

func TestBoundaryItemBaseIsolatedByRequestPurpose(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	coding := saveDelta(t, s, session, turn, "main", "key", nativeItems("A"))
	for _, purpose := range []string{"naming", "compaction"} {
		request, err := s.StartRequest(session.ID, turn, "main", purpose, session.Model)
		if err != nil {
			t.Fatal(err)
		}
		items, err := s.RecordBoundary(session.ID, turn, "main", request, "key", llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: json.RawMessage(`{}`), Inspection: nativeItems("A")})
		if err != nil {
			t.Fatal(err)
		}
		if record := deltaRecord(t, s, items[0].ID); record.Base != nil || record.DeltaEmpty {
			t.Fatalf("%s inherited a coding base: %+v", purpose, record)
		}
	}
	next := saveDelta(t, s, session, turn, "main", "new-cache-key", nativeItems("A"))
	record := deltaRecord(t, s, next.ID)
	if record.Base == nil || record.Base.Header != coding.ID || !record.DeltaEmpty {
		t.Fatalf("coding used another purpose's intervening request: %+v", record)
	}
}
