package history

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestRequestOutcomeUsesProviderNeutralState(t *testing.T) {
	for _, state := range []string{"completed", "failed", "interrupted"} {
		t.Run(state, func(t *testing.T) {
			s, session, turn, _ := historyFixture(t)
			request, err := s.StartRequest(session.ID, turn, "main", "coding", session.Model)
			if err != nil {
				t.Fatal(err)
			}
			for _, direction := range []string{"request", "outcome"} {
				doc := &llm.InspectionDocument{}
				if direction == "outcome" {
					doc.State = state
				}
				if _, err := s.RecordBoundary(session.ID, turn, "main", request, "state", llm.BoundaryEvent{Attempt: 1, Direction: direction, Payload: json.RawMessage(`{}`), Inspection: doc}); err != nil {
					t.Fatal(err)
				}
			}
			items, err := s.StreamItems(context.Background(), session.ID)
			if err != nil || len(items) != 1 || !strings.HasSuffix(items[0].Title, " · "+state) {
				t.Fatalf("state=%s items=%+v err=%v", state, items, err)
			}
		})
	}
}

func TestGroupedResponseHeadingDoesNotDuplicateOrMutateMetadata(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	if _, err := s.RecordBoundary(session.ID, turn, "main", request, "heading", llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: json.RawMessage(`{}`), Inspection: testInspection("input")}); err != nil {
		t.Fatal(err)
	}
	doc := &llm.InspectionDocument{Sections: []llm.InspectionSection{{Title: "Response", Fields: []llm.InspectionField{{Name: "text", Value: llm.InspectionValue{Kind: "string", Text: "output"}}}}}}
	items, err := s.RecordBoundary(session.ID, turn, "main", request, "heading", llm.BoundaryEvent{Attempt: 1, Direction: "response", Payload: json.RawMessage(`{"text":"output"}`), Inspection: doc})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Sections[0].Title != "Response" {
		t.Fatal("provider metadata was mutated")
	}
	for _, expanded := range []bool{false, true} {
		var page InspectionPage
		if expanded {
			page, err = s.BoundaryExpandedPage(context.Background(), items[0].Revision, 0, InspectionPageChars)
		} else {
			page, err = s.InspectPage(context.Background(), items[0].Revision, 0, InspectionPageChars)
		}
		human, _, _ := strings.Cut(page.Text, "Prettified raw JSON")
		if err != nil || strings.Count(human, "Response #1\n") != 1 || strings.Contains(human, "\nResponse\n") {
			t.Fatalf("expanded=%v duplicate response heading: %q err=%v", expanded, page.Text, err)
		}
	}
}
