package history

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestBoundaryCapturesQueryUsesAncestryLookups(t *testing.T) {
	s, _, _, _ := historyFixture(t)
	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+boundaryCapturesQuery, 1, 1, 0, 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	lookups := 0
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		// A full entries scan remains linear in all saved sessions, even though
		// the recursive CTE itself stops at this attempt's header.
		if strings.HasPrefix(detail, "SCAN e") {
			t.Fatalf("unbounded entries scan: %s", detail)
		}
		if strings.HasPrefix(detail, "SEARCH e USING INTEGER PRIMARY KEY") {
			lookups++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if lookups != 3 {
		t.Fatalf("expected primary-key lookups in seed, recursion and projection; got %d", lookups)
	}
}

// BenchmarkBoundaryCapturesUnrelatedHistory holds the inspected attempt fixed
// while adding entries outside its ancestry, as in a long-lived history DB.
func BenchmarkBoundaryCapturesUnrelatedHistory(b *testing.B) {
	for _, count := range []int{0, 10000} {
		b.Run(fmt.Sprintf("entries_%d", count), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "data"))
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			model := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
			session := NewID("session")
			turn, _, err := s.StartSession(session, b.TempDir(), model, llm.Message{Type: llm.TypeUserInput, Content: "hello"})
			if err != nil {
				b.Fatal(err)
			}
			request, err := s.StartRequest(session, turn, "main", "coding", model)
			if err != nil {
				b.Fatal(err)
			}
			items, err := s.RecordBoundary(session, turn, "main", request, "key", llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: []byte(`{"input":"test"}`)})
			if err != nil {
				b.Fatal(err)
			}
			// Insert a disconnected synthetic branch without costly per-row commits.
			if _, err := s.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 WHERE ?>0 UNION ALL SELECT i+1 FROM n WHERE i<?)
    INSERT INTO entries(session_id,actor_id,kind,model_visible,content_json,created_ms)
    SELECT ?,'main','status',0,?,0 FROM n`, count, count, session, `{"type":"fixture","text":"`+strings.Repeat("x", 4096)+`"}`); err != nil {
				b.Fatal(err)
			}
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				details, err := s.BoundaryCaptures(ctx, items[0].Revision)
				if err != nil || len(details) != 1 {
					b.Fatalf("captures=%d: %v", len(details), err)
				}
			}
		})
	}
}
