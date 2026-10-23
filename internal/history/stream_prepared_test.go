package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"ttc/internal/llm"
)

func TestPreparedBoundaryConcurrentCacheAndNearestStop(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	first := saveDelta(t, s, session, turn, "main", "", nativeItems("stable"))
	var walked int
	if err := s.db.QueryRow(boundaryNearestBase+`SELECT count(*) FROM ancestry`, "main", "coding", first.ID).Scan(&walked); err != nil {
		t.Fatal(err)
	}
	if walked != 1 {
		t.Fatalf("nearest base traversed %d rows beyond matching tip", walked)
	}
	event := llm.BoundaryEvent{Direction: "request", Attempt: 1, Payload: json.RawMessage(`{}`), Inspection: nativeItems("stable", "new")}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := s.PrepareBoundary(session.ID, request, event)
			if err == nil && p.baseID != first.ID {
				err = fmt.Errorf("wrong prepared base %d", p.baseID)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPreparedBoundaryRejectsChangedBaseAndRouting(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	first := saveDelta(t, s, session, turn, "main", "key", nativeItems("A"))
	request, err := s.StartRequest(session.ID, turn, "main", "coding", session.Model)
	if err != nil {
		t.Fatal(err)
	}
	event := llm.BoundaryEvent{Direction: "request", Attempt: 1, Payload: json.RawMessage(`{}`), Inspection: nativeItems("next")}
	prepared, err := s.PrepareBoundary(session.ID, request, event)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := saveDelta(t, s, session, turn, "main", "key", nativeItems("concurrent"))
	if _, err = s.RecordPreparedBoundary(session.ID, turn, "main", request, "key", prepared); !errors.Is(err, ErrBoundaryBaseChanged) {
		t.Fatal("accepted concurrent base", err)
	}
	if _, err = s.db.Exec("UPDATE sessions SET active_entry_id=? WHERE id=?", first.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	// Returning to the same immutable selected base is safe despite unrelated work.
	if _, err = s.RecordPreparedBoundary(session.ID, turn, "main", request, "key", prepared); err != nil {
		t.Fatal(err)
	}
	prepared, err = s.PrepareBoundary(session.ID, request, event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE sessions SET active_entry_id=? WHERE id=?", concurrent.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordPreparedBoundary(session.ID, turn, "main", request, "key", prepared); !errors.Is(err, ErrBoundaryBaseChanged) {
		t.Fatal("accepted selected-branch change", err)
	}
	if _, err = s.RecordPreparedBoundary(session.ID, turn, "main", request+1, "key", prepared); !errors.Is(err, ErrBoundaryBaseChanged) {
		t.Fatal("accepted request routing change", err)
	}
}

func TestPreparedBoundaryCommitDoesNotReadArtifacts(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	first := saveDelta(t, s, session, turn, "main", "", nativeItems("same"))
	request, err := s.StartRequest(session.ID, turn, "main", "coding", session.Model)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.PrepareBoundary(session.ID, request, llm.BoundaryEvent{Direction: "request", Attempt: 1, Payload: json.RawMessage(`{}`), Inspection: nativeItems("same", "new")})
	if err != nil {
		t.Fatal(err)
	}
	base := deltaRecord(t, s, first.ID)
	// All artifact and reference reads must already be complete. Commit should
	// only validate SQL ancestry and publish the ready immutable descriptor.
	removed := map[string]bool{}
	for _, path := range []string{base.Items, p.itemRefs, p.readable, p.expanded, p.prettyJSON} {
		if removed[path] {
			continue
		}
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
		removed[path] = true
	}
	if _, err = s.RecordPreparedBoundary(session.ID, turn, "main", request, "", p); err != nil {
		t.Fatal("commit performed artifact IO", err)
	}
}

func TestBoundaryItemCacheBoundsAndProjectionReuse(t *testing.T) {
	var c boundaryItemCache
	p := preparedItem{native: rawManifest{Chunks: make([]boundaryChunk, 40)}}
	for i := 0; i < boundaryItemCacheEntries*2; i++ {
		c.put(boundaryItemKey{hash: fmt.Sprint(i)}, p)
	}
	if len(c.entries) > boundaryItemCacheEntries || c.chunks > boundaryItemCacheChunks {
		t.Fatal(len(c.entries), c.chunks)
	}
	if _, ok := c.get(boundaryItemKey{hash: "0"}); ok {
		t.Fatal("old entry not evicted")
	}
	c.put(boundaryItemKey{hash: "oversized"}, preparedItem{native: rawManifest{Chunks: make([]boundaryChunk, boundaryItemCacheChunks+1)}})
	if _, ok := c.get(boundaryItemKey{hash: "oversized"}); ok {
		t.Fatal("oversized entry admitted")
	}
	s, session, _, request := historyFixture(t)
	doc := nativeItems("cached")
	event := llm.BoundaryEvent{Direction: "request", Attempt: 1, Payload: json.RawMessage(`{}`), Inspection: doc}
	first, err := s.PrepareBoundary(session.ID, request, event)
	if err != nil {
		t.Fatal(err)
	}
	var refs []boundaryItemRef
	b, err := readBoundaryCAS(first.itemRefs, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &refs); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(refs[0].Expanded); err != nil {
		t.Fatal(err)
	}
	// Cache hits compose cached indexes without reopening or reformatting the
	// per-item artifacts. Actual viewer reads still authenticate body chunks.
	second, err := s.PrepareBoundary(session.ID, request, event)
	if err != nil {
		t.Fatal(err)
	}
	if second.expanded != first.expanded {
		t.Fatal("stable projection not reused")
	}
	if len(s.boundaryItems.entries) != 1 {
		t.Fatal(len(s.boundaryItems.entries))
	}
	doc.Items[0].Name = "item 2"
	if _, err = s.PrepareBoundary(session.ID, request, event); err != nil {
		t.Fatal(err)
	}
	if len(s.boundaryItems.entries) != 2 {
		t.Fatal("display name missing from key")
	}
}
