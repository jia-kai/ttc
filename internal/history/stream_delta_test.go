package history

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/messagestream"
)

func nativeItems(values ...string) *llm.InspectionDocument {
	doc := &llm.InspectionDocument{Items: make([]llm.InspectionItem, 0, len(values))}
	for i, text := range values {
		raw, _ := json.Marshal(map[string]string{"native": text})
		doc.Items = append(doc.Items, llm.InspectionItem{Section: "Input", Name: fmt.Sprintf("item %d", i+1), Hash: messagestream.Hash(raw), JSON: raw, Value: llm.InspectionValue{Kind: "string", Text: text}})
	}
	return doc
}
func saveDelta(t *testing.T, s *Store, session Session, turn, actor, conversation string, doc *llm.InspectionDocument) messagestream.Item {
	t.Helper()
	request, err := s.StartRequest(session.ID, turn, actor, "coding", session.Model)
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.RecordBoundary(session.ID, turn, actor, request, conversation, llm.BoundaryEvent{Direction: "request", Attempt: 1, Payload: json.RawMessage(`{"exact":"FULL REQUEST NEVER IN DELTA JSON"}`), Inspection: doc})
	if err != nil {
		t.Fatal(err)
	}
	return items[0]
}
func deltaRecord(t *testing.T, s *Store, id int64) boundaryRecord {
	t.Helper()
	r, err := s.boundaryTarget(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestBoundaryItemDeltaMiddleAppendAndExact(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	first := saveDelta(t, s, session, turn, "main", "old-cache-key", nativeItems("A", "B", "C", "D", "E", "F"))
	second := saveDelta(t, s, session, turn, "main", "new-cache-key", nativeItems("A", "B", "X", "D", "E", "F", "G"))
	r := deltaRecord(t, s, second.ID)
	if r.Base == nil || r.Base.Header != first.ID {
		t.Fatal(r.Base)
	}
	all, err := s.boundaryGroupAll(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	human, native, _ := strings.Cut(all, "Prettified raw JSON")
	for _, text := range []string{"X", "G"} {
		if !strings.Contains(human, text) || !strings.Contains(native, `"native": "`+text+`"`) {
			t.Fatal(all)
		}
	}
	if !strings.Contains(human, "2 added, 1 removed, 5 unchanged") || !strings.Contains(human, "Removed:") {
		t.Fatal(human)
	}
	for _, text := range []string{"A", "B", "C", "D", "E", "F"} {
		if strings.Contains(native, `"native": "`+text+`"`) {
			t.Fatal("unchanged or removed JSON", native)
		}
	}
	if strings.Contains(all, "FULL REQUEST") {
		t.Fatal(all)
	}
	raw, err := s.BoundaryRaw(context.Background(), second.ID)
	if err != nil || !strings.Contains(string(raw), "FULL REQUEST") {
		t.Fatal(string(raw), err)
	}
	refs, err := readBoundaryCAS(r.Items, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	var items []boundaryItemRef
	if err = json.Unmarshal(refs, &items); err != nil || len(items) != 7 {
		t.Fatal(len(items), err)
	}
	third := saveDelta(t, s, session, turn, "main", "another-key", nativeItems("A", "B", "X", "D", "E", "F", "G"))
	all, err = s.boundaryGroupAll(context.Background(), third.ID)
	if err != nil || !strings.Contains(all, "No item changes.") || strings.Contains(all, "Prettified raw JSON") {
		t.Fatal(all, err)
	}
}
func TestBoundaryItemSelectedAncestryActorAndRetry(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	first := saveDelta(t, s, session, turn, "main", "key", nativeItems("A"))
	other := saveDelta(t, s, session, turn, "other", "key", nativeItems("other"))
	if deltaRecord(t, s, other.ID).Base != nil {
		t.Fatal("actor crossed")
	}
	sibling := saveDelta(t, s, session, turn, "main", "key", nativeItems("sibling"))
	if _, err := s.db.Exec("UPDATE sessions SET active_entry_id=? WHERE id=?", first.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	branch := saveDelta(t, s, session, turn, "main", "new-key", nativeItems("branch"))
	r := deltaRecord(t, s, branch.ID)
	if r.Base == nil || r.Base.Header != first.ID || r.Base.Header == sibling.ID {
		t.Fatal(r.Base)
	}
	retry, err := s.RecordBoundary(session.ID, turn, "main", r.Request, "retry-key", llm.BoundaryEvent{Direction: "request", Attempt: 2, Payload: json.RawMessage(`{}`), Inspection: nativeItems("branch")})
	if err != nil {
		t.Fatal(err)
	}
	r = deltaRecord(t, s, retry[0].ID)
	if r.Base == nil || r.Base.Header != branch.ID || !r.DeltaEmpty {
		t.Fatal(r)
	}
	// A detached selected root (/new semantics) cannot inherit any prior base.
	if _, err = s.db.Exec("UPDATE sessions SET active_entry_id=NULL WHERE id=?", session.ID); err != nil {
		t.Fatal(err)
	}
	fresh := saveDelta(t, s, session, turn, "main", "key", nativeItems("fresh"))
	if deltaRecord(t, s, fresh.ID).Base != nil {
		t.Fatal("detached root inherited base")
	}
}

func TestBoundaryItemBaselineEmptyAndLegacyReset(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	empty := saveDelta(t, s, session, turn, "main", "", nativeItems())
	r := deltaRecord(t, s, empty.ID)
	if r.Items == "" || !r.DeltaEmpty {
		t.Fatal(r)
	}
	saveDelta(t, s, session, turn, "main", "", testInspection("legacy no item extraction"))
	next := saveDelta(t, s, session, turn, "main", "", nativeItems("new"))
	all, err := s.boundaryGroupAll(context.Background(), next.ID)
	if err != nil || !strings.Contains(all, "previous capture has no item references") {
		t.Fatal(all, err)
	}
}
